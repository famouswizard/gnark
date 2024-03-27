package fflonk

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"math/bits"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/consensys/gnark-crypto/ecc"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	curve "github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fflonk"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/fft"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/hash_to_field"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/iop"

	"github.com/consensys/gnark-crypto/ecc/bn254/kzg"
	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/witness"

	"github.com/consensys/gnark/constraint"
	cs "github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/constraint/solver"
	fcs "github.com/consensys/gnark/frontend/cs"
	"github.com/consensys/gnark/internal/utils"
	"github.com/consensys/gnark/logger"
)

const (
	id_L int = iota
	id_R
	id_O
	id_Z
	id_ZS
	id_Ql
	id_Qr
	id_Qm
	id_Qo
	id_Qk
	id_S1
	id_S2
	id_S3
	id_ID
	id_LOne
	id_Qci // [ .. , Qc_i, Pi_i, ...]
)

// blinding factors
const (
	id_Bl int = iota
	id_Br
	id_Bo
	id_Bz
	nb_blinding_polynomials
)

// blinding orders (-1 to deactivate)
const (
	// order_blinding_L = 1
	// order_blinding_R = 1
	// order_blinding_O = 1
	// order_blinding_Z = 2
	order_blinding_L = -1
	order_blinding_R = -1
	order_blinding_O = -1
	order_blinding_Z = -1
)

type Proof struct {

	// Commitments to the solution vectors, entangled
	LROEntangled kzg.Digest

	// Non entangled commitment to Z
	Z kzg.Digest

	// Commitment to Z, the permutation polynomial (perpared to be entangled)
	ZEntangled kzg.Digest

	// Commitment to Z, not entangled
	// Z kzg.Digest

	// Commitments to h1, h2, h3 (entangled) such that h = h1 + Xⁿ⁺²*h2 + X²⁽ⁿ⁺²⁾*h3 is the quotient polynomial
	HEntangled kzg.Digest

	// commitment to the wires associated to the custom gates (entangled)
	Bsb22Commitments []kzg.Digest

	// fflonk batch opening proof of
	// * (Ql,Qr,Qm,Qo,QkIncomplete,S1,S2,S3,(SetupQcp)_i,L,R,O,(GateQcp)_i,Z,H₁,H₂,H₃) at ζᵗ where
	// t is the number of polynomials to open
	// * Z at ωζᵐ
	BatchOpeningProof fflonk.OpeningProof
}

func Prove(spr *cs.SparseR1CS, pk *ProvingKey, fullWitness witness.Witness, opts ...backend.ProverOption) (*Proof, error) {

	log := logger.Logger().With().
		Str("curve", spr.CurveID().String()).
		Int("nbConstraints", spr.GetNbConstraints()).
		Str("backend", "plonk").Logger()

	// parse the options
	opt, err := backend.NewProverConfig(opts...)
	if err != nil {
		return nil, fmt.Errorf("get prover options: %w", err)
	}

	start := time.Now()

	// init instance
	g, ctx := errgroup.WithContext(context.Background())
	instance, err := newInstance(ctx, spr, pk, fullWitness, &opt)
	if err != nil {
		return nil, fmt.Errorf("new instance: %w", err)
	}

	// solve constraints
	g.Go(instance.solveConstraints)

	// complete qk
	g.Go(instance.completeQk)

	// init blinding polynomials
	g.Go(instance.initBlindingPolynomials)

	// derive gamma, beta (copy constraint)
	g.Go(instance.deriveGammaAndBeta)

	// compute accumulating ratio for the copy constraint
	g.Go(instance.buildRatioCopyConstraint)

	// compute h
	g.Go(instance.computeQuotient)

	// open Z (blinded) at ωζ (proof.ZShiftedOpening)

	// Batch opening
	g.Go(instance.batchOpening)

	if err := g.Wait(); err != nil {
		return nil, err
	}

	log.Debug().Dur("took", time.Since(start)).Msg("prover done")
	return instance.proof, nil
}

// represents a Prover instance
type instance struct {
	ctx context.Context

	pk    *ProvingKey
	proof *Proof
	spr   *cs.SparseR1CS
	opt   *backend.ProverConfig

	fs             *fiatshamir.Transcript
	kzgFoldingHash hash.Hash // for KZG folding
	htfFunc        hash.Hash // hash to field function

	// polynomials
	x        []*iop.Polynomial // x stores tracks the polynomial we need
	bp       []*iop.Polynomial // blinding polynomials
	h        *iop.Polynomial   // h is the quotient polynomial
	blindedZ []fr.Element      // blindedZ is the blinded version of Z

	fullWitness witness.Witness

	// bsb22 commitment stuff
	commitmentInfo constraint.PlonkCommitments
	commitmentVal  []fr.Element
	cCommitments   []*iop.Polynomial

	// challenges
	gamma, beta, alpha, zeta fr.Element

	// channel to wait for the steps
	chLRO,
	chQk,
	chbp,
	chZ,
	chH,
	chRestoreLRO,
	chGammaBeta chan struct{}

	domain0, domain1 *fft.Domain

	trace *Trace
}

func newInstance(ctx context.Context, spr *cs.SparseR1CS, pk *ProvingKey, fullWitness witness.Witness, opts *backend.ProverConfig) (*instance, error) {
	if opts.HashToFieldFn == nil {
		opts.HashToFieldFn = hash_to_field.New([]byte("BSB22-Plonk"))
	}
	s := instance{
		ctx:            ctx,
		pk:             pk,
		proof:          &Proof{},
		spr:            spr,
		opt:            opts,
		fullWitness:    fullWitness,
		bp:             make([]*iop.Polynomial, nb_blinding_polynomials),
		fs:             fiatshamir.NewTranscript(opts.ChallengeHash, "gamma", "beta", "alpha", "zeta"),
		kzgFoldingHash: opts.KZGFoldingHash,
		htfFunc:        opts.HashToFieldFn,
		chLRO:          make(chan struct{}, 1),
		chQk:           make(chan struct{}, 1),
		chbp:           make(chan struct{}, 1),
		chGammaBeta:    make(chan struct{}, 1),
		chZ:            make(chan struct{}, 1),
		chH:            make(chan struct{}, 1),
		chRestoreLRO:   make(chan struct{}, 1),
	}
	s.initBSB22Commitments()
	s.x = make([]*iop.Polynomial, id_Qci+2*len(s.commitmentInfo))

	// init fft domains
	nbConstraints := spr.GetNbConstraints()
	sizeSystem := uint64(nbConstraints + len(spr.Public)) // len(spr.Public) is for the placeholder constraints
	s.domain0 = fft.NewDomain(sizeSystem)

	// h, the quotient polynomial is of degree 3(n+1)+2, so it's in a 3(n+2) dim vector space,
	// the domain is the next power of 2 superior to 3(n+2). 4*domainNum is enough in all cases
	// except when n<6.
	if sizeSystem < 6 {
		s.domain1 = fft.NewDomain(8*sizeSystem, fft.WithoutPrecompute())
	} else {
		s.domain1 = fft.NewDomain(4*sizeSystem, fft.WithoutPrecompute())
	}
	// TODO @gbotrel domain1 is used for only 1 FFT → precomputing the twiddles
	// and storing them in memory is costly given its size. → do a FFT on the fly

	// build trace
	s.trace = NewTrace(spr, s.domain0)

	return &s, nil
}

func (s *instance) initBlindingPolynomials() error {
	s.bp[id_Bl] = getRandomPolynomial(order_blinding_L)
	s.bp[id_Br] = getRandomPolynomial(order_blinding_R)
	s.bp[id_Bo] = getRandomPolynomial(order_blinding_O)
	s.bp[id_Bz] = getRandomPolynomial(order_blinding_Z)
	close(s.chbp)
	return nil
}

func (s *instance) initBSB22Commitments() {
	s.commitmentInfo = s.spr.CommitmentInfo.(constraint.PlonkCommitments)
	s.commitmentVal = make([]fr.Element, len(s.commitmentInfo)) // TODO @Tabaie get rid of this
	s.cCommitments = make([]*iop.Polynomial, len(s.commitmentInfo))
	s.proof.Bsb22Commitments = make([]kzg.Digest, len(s.commitmentInfo))

	// override the hint for the commitment constraints
	bsb22ID := solver.GetHintID(fcs.Bsb22CommitmentComputePlaceholder)
	s.opt.SolverOpts = append(s.opt.SolverOpts, solver.OverrideHint(bsb22ID, s.bsb22Hint))
}

// Computing and verifying Bsb22 multi-commits explained in https://hackmd.io/x8KsadW3RRyX7YTCFJIkHg
func (s *instance) bsb22Hint(_ *big.Int, ins, outs []*big.Int) error {
	var err error
	commDepth := int(ins[0].Int64())
	ins = ins[1:]

	res := &s.commitmentVal[commDepth]

	commitmentInfo := s.spr.CommitmentInfo.(constraint.PlonkCommitments)[commDepth]
	committedValues := make([]fr.Element, s.domain0.Cardinality)
	offset := s.spr.GetNbPublicVariables()
	for i := range ins {
		committedValues[offset+commitmentInfo.Committed[i]].SetBigInt(ins[i])
	}
	if _, err = committedValues[offset+commitmentInfo.CommitmentIndex].SetRandom(); err != nil { // Commitment injection constraint has qcp = 0. Safe to use for blinding.
		return err
	}
	if _, err = committedValues[offset+s.spr.GetNbConstraints()-1].SetRandom(); err != nil { // Last constraint has qcp = 0. Safe to use for blinding
		return err
	}
	s.cCommitments[commDepth] = iop.NewPolynomial(&committedValues, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})

	// /!\ LAGRANGE
	// if s.proof.Bsb22Commitments[commDepth], err = kzg.Commit(s.cCommitments[commDepth].Coefficients(), s.pk.KzgLagrange); err != nil {
	// 	return err
	// }
	s.cCommitments[commDepth].ToCanonical(s.domain0).ToRegular()
	var subPk kzg.ProvingKey
	subPk.G1 = make([]curve.G1Affine, len(s.cCommitments[commDepth].Coefficients()))
	t := getNextDivisorRMinusOne(*s.pk.Vk)
	id := setup_s3 + 1 + commDepth
	for i := 0; i < len(s.cCommitments[commDepth].Coefficients()); i++ {
		subPk.G1[i].Set(&s.pk.Kzg.G1[i*t+id])
	}
	if s.proof.Bsb22Commitments[commDepth], err = kzg.Commit(s.cCommitments[commDepth].Coefficients(), subPk); err != nil {
		return err
	}
	s.cCommitments[commDepth].ToLagrange(s.domain0).ToRegular()

	s.htfFunc.Write(s.proof.Bsb22Commitments[commDepth].Marshal())
	hashBts := s.htfFunc.Sum(nil)
	s.htfFunc.Reset()
	nbBuf := fr.Bytes
	if s.htfFunc.Size() < fr.Bytes {
		nbBuf = s.htfFunc.Size()
	}
	res.SetBytes(hashBts[:nbBuf]) // TODO @Tabaie use CommitmentIndex for this; create a new variable CommitmentConstraintIndex for other uses
	res.BigInt(outs[0])

	return nil
}

// solveConstraints computes the evaluation of the polynomials L, R, O
// and sets x[id_L], x[id_R], x[id_O] in canonical form
func (s *instance) solveConstraints() error {
	_solution, err := s.spr.Solve(s.fullWitness, s.opt.SolverOpts...)
	if err != nil {
		return err
	}
	solution := _solution.(*cs.SparseR1CSSolution)
	evaluationLDomainSmall := []fr.Element(solution.L)
	evaluationRDomainSmall := []fr.Element(solution.R)
	evaluationODomainSmall := []fr.Element(solution.O)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		s.x[id_L] = iop.NewPolynomial(&evaluationLDomainSmall, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
		wg.Done()
	}()
	go func() {
		s.x[id_R] = iop.NewPolynomial(&evaluationRDomainSmall, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
		wg.Done()
	}()

	s.x[id_O] = iop.NewPolynomial(&evaluationODomainSmall, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})

	wg.Wait()

	// commit to l, r, o and add blinding factors
	if err := s.commitToLRO(); err != nil {
		return err
	}
	close(s.chLRO)
	return nil
}

func (s *instance) completeQk() error {
	qk := s.trace.Qk.Clone()
	qkCoeffs := qk.Coefficients()

	wWitness, ok := s.fullWitness.Vector().(fr.Vector)
	if !ok {
		return witness.ErrInvalidWitness
	}

	copy(qkCoeffs, wWitness[:len(s.spr.Public)])

	// wait for solver to be done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}

	for i := range s.commitmentInfo {
		qkCoeffs[s.spr.GetNbPublicVariables()+s.commitmentInfo[i].CommitmentIndex] = s.commitmentVal[i]
	}

	s.x[id_Qk] = qk
	close(s.chQk)

	return nil
}

func (s *instance) commitToLRO() error {

	// wait for blinding polynomials to be initialized or context to be done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chbp:
	}

	g := new(errgroup.Group)
	offsetQcp := len(s.pk.Vk.Qcp)

	var l, r, o kzg.Digest

	g.Go(func() (err error) {
		l, err = s.commitToEntangledPolyAndBlinding(s.x[id_L], s.bp[id_Bl], prover_l+offsetQcp)
		return
	})

	g.Go(func() (err error) {
		r, err = s.commitToEntangledPolyAndBlinding(s.x[id_R], s.bp[id_Br], prover_r+offsetQcp)
		return
	})

	g.Go(func() (err error) {
		o, err = s.commitToEntangledPolyAndBlinding(s.x[id_O], s.bp[id_Bo], prover_o+offsetQcp)
		return
	})

	s.proof.LROEntangled.Add(&l, &r).Add(&s.proof.LROEntangled, &o)

	return g.Wait()
}

// deriveGammaAndBeta (copy constraint)
func (s *instance) deriveGammaAndBeta() error {
	wWitness, ok := s.fullWitness.Vector().(fr.Vector)
	if !ok {
		return witness.ErrInvalidWitness
	}

	if err := bindPublicData(s.fs, "gamma", s.pk.Vk, wWitness[:len(s.spr.Public)]); err != nil {
		return err
	}

	// wait for LRO to be committed
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}

	gamma, err := deriveRandomness(s.fs, "gamma", &s.proof.LROEntangled)
	if err != nil {
		return err
	}

	bbeta, err := s.fs.ComputeChallenge("beta")
	if err != nil {
		return err
	}
	s.gamma = gamma
	s.beta.SetBytes(bbeta)

	close(s.chGammaBeta)

	return nil
}

// commitToPolyAndBlinding computes the KZG commitment of a polynomial p
// in Lagrange form (large degree)
// and add the contribution of a blinding polynomial b (small degree)
// /!\ The polynomial p is supposed to be in Lagrange form.
func (s *instance) commitToPolyAndBlinding(p, b *iop.Polynomial) (commit curve.G1Affine, err error) {

	p.ToCanonical(s.domain0).ToRegular()
	commit, err = kzg.Commit(p.Coefficients(), s.pk.Kzg)

	// we add in the blinding contribution
	n := int(s.domain0.Cardinality)
	cb := commitBlindingFactor(n, b, s.pk.Kzg)
	commit.Add(&commit, &cb)

	return
}

// commitToEntangledPolyAndBlinding computes the KZG commitment of a polynomial p
// in Lagrange form (large degree)
// and add the contribution of a blinding polynomial b (small degree)
// /!\ The polynomial p is supposed to be in Lagrange form.
// id indicates the entangled position of p in the folded commitment
func (s *instance) commitToEntangledPolyAndBlinding(p, b *iop.Polynomial, id int) (commit curve.G1Affine, err error) {

	// LAGRANGE
	// commit, err = kzg.Commit(p.Coefficients(), s.pk.KzgLagrange)
	p.ToCanonical(s.domain0).ToRegular()

	// get the sub KZG proving key
	t := getNextDivisorRMinusOne(*s.pk.Vk)
	var subPk kzg.ProvingKey
	subPk.G1 = make([]bn254.G1Affine, len(p.Coefficients()))
	for i := 0; i < len(p.Coefficients()); i++ {
		subPk.G1[i].Set(&s.pk.Kzg.G1[i*t+id])
	}

	commit, err = kzg.Commit(p.Coefficients(), subPk)
	p.ToLagrange(s.domain0)

	// we add in the blinding contribution
	n := int(s.domain0.Cardinality)
	cb := commitBlindingFactor(n, b, s.pk.Kzg)
	commit.Add(&commit, &cb)

	return
}

func (s *instance) deriveAlpha() (err error) {
	alphaDeps := make([]*curve.G1Affine, len(s.proof.Bsb22Commitments)+1)
	for i := range s.proof.Bsb22Commitments {
		alphaDeps[i] = &s.proof.Bsb22Commitments[i]
	}
	alphaDeps[len(alphaDeps)-1] = &s.proof.ZEntangled
	s.alpha, err = deriveRandomness(s.fs, "alpha", alphaDeps...)
	return err
}

func (s *instance) deriveZeta() (err error) {
	s.zeta, err = deriveRandomness(s.fs, "zeta", &s.proof.HEntangled)
	return
}

// computeQuotient computes H
func (s *instance) computeQuotient() (err error) {
	s.x[id_Ql] = s.trace.Ql
	s.x[id_Qr] = s.trace.Qr
	s.x[id_Qm] = s.trace.Qm
	s.x[id_Qo] = s.trace.Qo
	s.x[id_S1] = s.trace.S1
	s.x[id_S2] = s.trace.S2
	s.x[id_S3] = s.trace.S3

	for i := 0; i < len(s.commitmentInfo); i++ {
		s.x[id_Qci+2*i] = s.trace.Qcp[i]
	}

	n := s.domain0.Cardinality
	lone := make([]fr.Element, n)
	lone[0].SetOne()

	// wait for solver to be done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}

	for i := 0; i < len(s.commitmentInfo); i++ {
		s.x[id_Qci+2*i+1] = s.cCommitments[i]
	}

	// wait for Z to be committed or context done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chZ:
	}

	// derive alpha
	if err = s.deriveAlpha(); err != nil {
		return err
	}

	// TODO complete waste of memory find another way to do that
	identity := make([]fr.Element, n)
	identity[1].Set(&s.beta)

	s.x[id_ID] = iop.NewPolynomial(&identity, iop.Form{Basis: iop.Canonical, Layout: iop.Regular})
	s.x[id_LOne] = iop.NewPolynomial(&lone, iop.Form{Basis: iop.Lagrange, Layout: iop.Regular})
	s.x[id_ZS] = s.x[id_Z].ShallowClone().Shift(1)

	numerator, err := s.computeNumerator()
	if err != nil {
		return err
	}

	s.h, err = divideByXMinusOne(numerator, [2]*fft.Domain{s.domain0, s.domain1})
	if err != nil {
		return err
	}

	// commit to h
	if err := s.commitToQuotient(s.h1(), s.h2(), s.h3(), s.proof, s.pk.Kzg); err != nil {
		return err
	}

	if err := s.deriveZeta(); err != nil {
		return err
	}

	// wait for clean up tasks to be done
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chRestoreLRO:
	}

	close(s.chH)

	return nil
}

func (s *instance) buildRatioCopyConstraint() (err error) {
	// wait for gamma and beta to be derived (or ctx.Done())
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chGammaBeta:
	}

	// TODO @gbotrel having iop.BuildRatioCopyConstraint return something
	// with capacity = len() + 4 would avoid extra alloc / copy during openZ
	s.x[id_Z], err = iop.BuildRatioCopyConstraint(
		[]*iop.Polynomial{
			s.x[id_L],
			s.x[id_R],
			s.x[id_O],
		},
		s.trace.S,
		s.beta,
		s.gamma,
		iop.Form{Basis: iop.Lagrange, Layout: iop.Regular},
		s.domain0,
	)
	if err != nil {
		return err
	}

	// commit to the blinded version of z (entangled)
	offsetQcp := len(s.pk.Vk.Qcp)
	s.proof.ZEntangled, err = s.commitToEntangledPolyAndBlinding(s.x[id_Z], s.bp[id_Bz], prover_z+offsetQcp)
	if err != nil {
		return err
	}
	s.proof.Z, err = s.commitToPolyAndBlinding(s.x[id_Z], s.bp[id_Z])

	close(s.chZ)

	return
}

func (s *instance) h1() []fr.Element {
	h1 := s.h.Coefficients()[:s.domain0.Cardinality+2]
	return h1
}

func (s *instance) h2() []fr.Element {
	h2 := s.h.Coefficients()[s.domain0.Cardinality+2 : 2*(s.domain0.Cardinality+2)]
	return h2
}

func (s *instance) h3() []fr.Element {
	h3 := s.h.Coefficients()[2*(s.domain0.Cardinality+2) : 3*(s.domain0.Cardinality+2)]
	return h3
}

func (s *instance) batchOpening() error {

	// wait for LRO to be committed (or ctx.Done())
	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chLRO:
	}

	select {
	case <-s.ctx.Done():
		return errContextDone
	case <-s.chH:
	}

	// step 0: compute the folded digest, which corresponds to the polynomial
	// Q_{public}+Qₗᵣₒ+Q_{Z}+Q_{H}
	var foldedDigests [2]kzg.Digest
	foldedDigests[0].Set(&s.pk.Vk.Qpublic).
		Add(&foldedDigests[0], &s.proof.LROEntangled).
		Add(&foldedDigests[0], &s.proof.ZEntangled).
		Add(&foldedDigests[0], &s.proof.HEntangled)
	for i := 0; i < len(s.proof.Bsb22Commitments); i++ {
		foldedDigests[0].Add(&foldedDigests[0], &s.proof.Bsb22Commitments[i])
	}
	foldedDigests[1].Set(&s.proof.Z)

	// step 1: prepare all the polynomials, there are two sets of polynomials:
	// Set 1 = {Ql,Qr,Qm,Qo,Qk,S₁,S₂,S₃,(Qcp)_i,L,R,O,H₁,H₂,H₃,(BSB)_i}
	// Set 2 = {Z}
	polysToOpen := make([][][]fr.Element, 2)
	t := getNextDivisorRMinusOne(*s.pk.Vk)
	polysToOpen[0] = make([][]fr.Element, t)
	polysToOpen[0][setup_ql] = s.trace.Ql.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_qr] = s.trace.Qr.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_qm] = s.trace.Qm.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_qo] = s.trace.Qo.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_qk_incomplete] = s.trace.Qk.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_s1] = s.trace.S1.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_s2] = s.trace.S2.ToCanonical(s.domain0).ToRegular().Coefficients()
	polysToOpen[0][setup_s3] = s.trace.S3.ToCanonical(s.domain0).ToRegular().Coefficients()
	for i := 0; i < len(s.trace.Qcp); i++ {
		polysToOpen[0][setup_s3+1+i] = s.trace.Qcp[i].ToCanonical(s.domain0).ToRegular().Coefficients()
	}
	offset := len(s.trace.Qcp)
	polysToOpen[0][prover_l+offset] = getBlindedCoefficients(s.x[id_L], s.bp[id_Bl])
	polysToOpen[0][prover_r+offset] = getBlindedCoefficients(s.x[id_R], s.bp[id_Br])
	polysToOpen[0][prover_o+offset] = getBlindedCoefficients(s.x[id_O], s.bp[id_Bo])
	polysToOpen[0][prover_z+offset] = getBlindedCoefficients(s.x[id_Z], s.bp[id_Bz])
	polysToOpen[0][prover_h_1+offset] = s.h1()
	polysToOpen[0][prover_h_2+offset] = s.h2()
	polysToOpen[0][prover_h_3+offset] = s.h3()
	for i := 0; i < len(s.trace.Qcp); i++ {
		polysToOpen[0][number_polynomials+offset+i] = s.x[id_Qci+2*i+1].ToCanonical(s.domain0).ToRegular().Coefficients()
	}

	polysToOpen[1] = make([][]fr.Element, 1)
	polysToOpen[1][0] = getBlindedCoefficients(s.x[id_Z], s.bp[id_Bz])

	// step 2: prepare the points, there are two sets (one for each set of polynomials)
	// set 1: {ζ}
	// set 2: {ωζ^{15+2*|Qcp|}}
	// The opening of the first set is done using fflonk, and the size of the first set of
	// polynomial is 15+2*|Qcp| so the the claimed values in the fllonk proof are the evaluations
	// of the polynomial in this set at ζ^{15+2*|Qcp|}.
	var omegaZetaT fr.Element
	var tBigInt big.Int
	tBigInt.SetUint64(uint64(t))
	omegaZetaT.Exp(s.zeta, &tBigInt).Mul(&omegaZetaT, &s.domain0.Generator)
	points := make([][]fr.Element, 2)
	points[0] = make([]fr.Element, 1)
	points[0][0].Set(&s.zeta)
	points[1] = make([]fr.Element, 1)
	points[1][0].Set(&omegaZetaT)

	// step 3: batch open
	var err error
	s.proof.BatchOpeningProof, err = fflonk.BatchOpen(polysToOpen, foldedDigests[:], points, s.opt.KZGFoldingHash, s.pk.Kzg)

	return err
}

// evaluate the full set of constraints, all polynomials in x are back in
// canonical regular form at the end
func (s *instance) computeNumerator() (*iop.Polynomial, error) {
	// init vectors that are used multiple times throughout the computation
	n := s.domain0.Cardinality
	twiddles0 := make([]fr.Element, n)
	if n == 1 {
		// edge case
		twiddles0[0].SetOne()
	} else {
		twiddles, err := s.domain0.Twiddles()
		if err != nil {
			return nil, err
		}
		copy(twiddles0, twiddles[0])
		w := twiddles0[1]
		for i := len(twiddles[0]); i < len(twiddles0); i++ {
			twiddles0[i].Mul(&twiddles0[i-1], &w)
		}
	}

	// wait for chQk to be closed (or ctx.Done())
	select {
	case <-s.ctx.Done():
		return nil, errContextDone
	case <-s.chQk:
	}

	nbBsbGates := (len(s.x) - id_Qci + 1) >> 1

	gateConstraint := func(u ...fr.Element) fr.Element {

		var ic, tmp fr.Element

		ic.Mul(&u[id_Ql], &u[id_L])
		tmp.Mul(&u[id_Qr], &u[id_R])
		ic.Add(&ic, &tmp)
		tmp.Mul(&u[id_Qm], &u[id_L]).Mul(&tmp, &u[id_R])
		ic.Add(&ic, &tmp)
		tmp.Mul(&u[id_Qo], &u[id_O])
		ic.Add(&ic, &tmp).Add(&ic, &u[id_Qk])
		for i := 0; i < nbBsbGates; i++ {
			tmp.Mul(&u[id_Qci+2*i], &u[id_Qci+2*i+1])
			ic.Add(&ic, &tmp)
		}

		return ic
	}

	var cs, css fr.Element
	cs.Set(&s.domain1.FrMultiplicativeGen)
	css.Square(&cs)

	orderingConstraint := func(u ...fr.Element) fr.Element {
		gamma := s.gamma

		var a, b, c, r, l fr.Element

		a.Add(&gamma, &u[id_L]).Add(&a, &u[id_ID])
		b.Mul(&u[id_ID], &cs).Add(&b, &u[id_R]).Add(&b, &gamma)
		c.Mul(&u[id_ID], &css).Add(&c, &u[id_O]).Add(&c, &gamma)
		r.Mul(&a, &b).Mul(&r, &c).Mul(&r, &u[id_Z])

		a.Add(&u[id_S1], &u[id_L]).Add(&a, &gamma)
		b.Add(&u[id_S2], &u[id_R]).Add(&b, &gamma)
		c.Add(&u[id_S3], &u[id_O]).Add(&c, &gamma)
		l.Mul(&a, &b).Mul(&l, &c).Mul(&l, &u[id_ZS])

		l.Sub(&l, &r)

		return l
	}

	ratioLocalConstraint := func(u ...fr.Element) fr.Element {

		var res fr.Element
		res.SetOne()
		res.Sub(&u[id_Z], &res).Mul(&res, &u[id_LOne])

		return res
	}

	rho := int(s.domain1.Cardinality / n)
	shifters := make([]fr.Element, rho)
	shifters[0].Set(&s.domain1.FrMultiplicativeGen)
	for i := 1; i < rho; i++ {
		shifters[i].Set(&s.domain1.Generator)
	}

	// stores the current coset shifter
	var coset fr.Element
	coset.SetOne()

	var tmp, one fr.Element
	one.SetOne()
	bn := big.NewInt(int64(n))

	cosetTable, err := s.domain0.CosetTable()
	if err != nil {
		return nil, err
	}

	// init the result polynomial & buffer
	cres := make([]fr.Element, s.domain1.Cardinality)
	buf := make([]fr.Element, n)
	var wgBuf sync.WaitGroup

	allConstraints := func(i int, u ...fr.Element) fr.Element {
		// scale S1, S2, S3 by β
		u[id_S1].Mul(&u[id_S1], &s.beta)
		u[id_S2].Mul(&u[id_S2], &s.beta)
		u[id_S3].Mul(&u[id_S3], &s.beta)

		// blind L, R, O, Z, ZS
		var y fr.Element
		y = s.bp[id_Bl].Evaluate(twiddles0[i])
		u[id_L].Add(&u[id_L], &y)
		y = s.bp[id_Br].Evaluate(twiddles0[i])
		u[id_R].Add(&u[id_R], &y)
		y = s.bp[id_Bo].Evaluate(twiddles0[i])
		u[id_O].Add(&u[id_O], &y)
		y = s.bp[id_Bz].Evaluate(twiddles0[i])
		u[id_Z].Add(&u[id_Z], &y)

		// ZS is shifted by 1; need to get correct twiddle
		y = s.bp[id_Bz].Evaluate(twiddles0[(i+1)%int(n)])
		u[id_ZS].Add(&u[id_ZS], &y)

		a := gateConstraint(u...)
		b := orderingConstraint(u...)
		c := ratioLocalConstraint(u...)
		c.Mul(&c, &s.alpha).Add(&c, &b).Mul(&c, &s.alpha).Add(&c, &a)
		return c
	}

	// for the first iteration, the scalingVector is the coset table
	scalingVector := cosetTable
	scalingVectorRev := make([]fr.Element, len(cosetTable))
	copy(scalingVectorRev, cosetTable)
	fft.BitReverse(scalingVectorRev)

	// pre-computed to compute the bit reverse index
	// of the result polynomial
	m := uint64(s.domain1.Cardinality)
	mm := uint64(64 - bits.TrailingZeros64(m))

	for i := 0; i < rho; i++ {

		coset.Mul(&coset, &shifters[i])
		tmp.Exp(coset, bn).Sub(&tmp, &one)

		// bl <- bl *( (s*ωⁱ)ⁿ-1 )s
		for _, q := range s.bp {
			cq := q.Coefficients()
			acc := tmp
			for j := 0; j < len(cq); j++ {
				cq[j].Mul(&cq[j], &acc)
				acc.Mul(&acc, &shifters[i])
			}
		}
		if i == 1 {
			// we have to update the scalingVector; instead of scaling by
			// cosets we scale by the twiddles of the large domain.
			w := s.domain1.Generator
			scalingVector = make([]fr.Element, n)
			fft.BuildExpTable(w, scalingVector)

			// reuse memory
			copy(scalingVectorRev, scalingVector)
			fft.BitReverse(scalingVectorRev)
		}

		// we do **a lot** of FFT here, but on the small domain.
		// note that for all the polynomials in the proving key
		// (Ql, Qr, Qm, Qo, S1, S2, S3, Qcp, Qc) and ID, LOne
		// we could pre-compute theses rho*2 FFTs and store them
		// at the cost of a huge memory footprint.
		batchApply(s.x, func(p *iop.Polynomial) {
			nbTasks := calculateNbTasks(len(s.x)-1) * 2
			// shift polynomials to be in the correct coset
			p.ToCanonical(s.domain0, nbTasks)

			// scale by shifter[i]
			var w []fr.Element
			if p.Layout == iop.Regular {
				w = scalingVector
			} else {
				w = scalingVectorRev
			}

			cp := p.Coefficients()
			utils.Parallelize(len(cp), func(start, end int) {
				for j := start; j < end; j++ {
					cp[j].Mul(&cp[j], &w[j])
				}
			}, nbTasks)

			// fft in the correct coset
			p.ToLagrange(s.domain0, nbTasks).ToRegular()
		})

		wgBuf.Wait()
		if _, err := iop.Evaluate(
			allConstraints,
			buf,
			iop.Form{Basis: iop.Lagrange, Layout: iop.Regular},
			s.x...,
		); err != nil {
			return nil, err
		}
		wgBuf.Add(1)
		go func(i int) {
			for j := 0; j < int(n); j++ {
				// we build the polynomial in bit reverse order
				cres[bits.Reverse64(uint64(rho*j+i))>>mm] = buf[j]
			}
			wgBuf.Done()
		}(i)

		tmp.Inverse(&tmp)
		// bl <- bl *( (s*ωⁱ)ⁿ-1 )s
		for _, q := range s.bp {
			cq := q.Coefficients()
			for j := 0; j < len(cq); j++ {
				cq[j].Mul(&cq[j], &tmp)
			}
		}
	}

	// scale everything back
	go func() {
		s.x[id_ID] = nil
		s.x[id_LOne] = nil
		s.x[id_ZS] = nil
		s.x[id_Qk] = nil

		var cs fr.Element
		cs.Set(&shifters[0])
		for i := 1; i < len(shifters); i++ {
			cs.Mul(&cs, &shifters[i])
		}
		cs.Inverse(&cs)

		batchApply(s.x, func(p *iop.Polynomial) {
			if p == nil {
				return
			}
			p.ToCanonical(s.domain0, 8).ToRegular()
			scalePowers(p, cs)
		})

		for _, q := range s.bp {
			scalePowers(q, cs)
		}

		close(s.chRestoreLRO)
	}()

	// ensure all the goroutines are done
	wgBuf.Wait()

	res := iop.NewPolynomial(&cres, iop.Form{Basis: iop.LagrangeCoset, Layout: iop.BitReverse})

	return res, nil

}

func calculateNbTasks(n int) int {
	nbAvailableCPU := runtime.NumCPU() - n
	if nbAvailableCPU < 0 {
		nbAvailableCPU = 1
	}
	nbTasks := 1 + (nbAvailableCPU / n)
	return nbTasks
}

// batchApply executes fn on all polynomials in x except x[id_ZS] in parallel.
func batchApply(x []*iop.Polynomial, fn func(*iop.Polynomial)) {
	var wg sync.WaitGroup
	for i := 0; i < len(x); i++ {
		if i == id_ZS {
			continue
		}
		wg.Add(1)
		go func(i int) {
			fn(x[i])
			wg.Done()
		}(i)
	}
	wg.Wait()
}

// p <- <p, (1, w, .., wⁿ) >
// p is supposed to be in canonical form
func scalePowers(p *iop.Polynomial, w fr.Element) {
	var acc fr.Element
	acc.SetOne()
	cp := p.Coefficients()
	for i := 0; i < p.Size(); i++ {
		cp[i].Mul(&cp[i], &acc)
		acc.Mul(&acc, &w)
	}
}

func evaluateBlinded(p, bp *iop.Polynomial, zeta fr.Element) fr.Element {
	// Get the size of the polynomial
	n := big.NewInt(int64(p.Size()))

	var pEvaluatedAtZeta fr.Element

	// Evaluate the polynomial and blinded polynomial at zeta
	chP := make(chan struct{}, 1)
	go func() {
		pEvaluatedAtZeta = p.Evaluate(zeta)
		close(chP)
	}()

	bpEvaluatedAtZeta := bp.Evaluate(zeta)

	// Multiply the evaluated blinded polynomial by tempElement
	var t fr.Element
	one := fr.One()
	t.Exp(zeta, n).Sub(&t, &one)
	bpEvaluatedAtZeta.Mul(&bpEvaluatedAtZeta, &t)

	// Add the evaluated polynomial and the evaluated blinded polynomial
	<-chP
	pEvaluatedAtZeta.Add(&pEvaluatedAtZeta, &bpEvaluatedAtZeta)

	// Return the result
	return pEvaluatedAtZeta
}

// /!\ modifies p's underlying array of coefficients, in particular the size changes
func getBlindedCoefficients(p, bp *iop.Polynomial) []fr.Element {
	cp := p.Coefficients()
	cbp := bp.Coefficients()
	cp = append(cp, cbp...)
	for i := 0; i < len(cbp); i++ {
		cp[i].Sub(&cp[i], &cbp[i])
	}
	return cp
}

// commits to a polynomial of the form b*(Xⁿ-1) where b is of small degree
func commitBlindingFactor(n int, b *iop.Polynomial, key kzg.ProvingKey) curve.G1Affine {
	cp := b.Coefficients()
	np := b.Size()

	// lo
	var tmp curve.G1Affine
	tmp.MultiExp(key.G1[:np], cp, ecc.MultiExpConfig{})

	// hi
	var res curve.G1Affine
	res.MultiExp(key.G1[n:n+np], cp, ecc.MultiExpConfig{})
	res.Sub(&res, &tmp)
	return res
}

// return a random polynomial of degree n, if n==-1 cancel the blinding
func getRandomPolynomial(n int) *iop.Polynomial {
	var a []fr.Element
	if n == -1 {
		a := make([]fr.Element, 1)
		a[0].SetZero()
	} else {
		a = make([]fr.Element, n+1)
		for i := 0; i <= n; i++ {
			a[i].SetRandom()
		}
	}
	res := iop.NewPolynomial(&a, iop.Form{
		Basis: iop.Canonical, Layout: iop.Regular})
	return res
}

func coefficients(p []*iop.Polynomial) [][]fr.Element {
	res := make([][]fr.Element, len(p))
	for i, pI := range p {
		res[i] = pI.Coefficients()
	}
	return res
}

func (s *instance) commitToQuotient(h1, h2, h3 []fr.Element, proof *Proof, kzgPk kzg.ProvingKey) error {
	g := new(errgroup.Group)
	t := number_polynomials + 2*len(s.pk.Vk.Qcp)
	lenQcp := len(s.pk.Vk.Qcp)
	rh1 := lenQcp + prover_h_1
	rh2 := lenQcp + prover_h_2
	rh3 := lenQcp + prover_h_3

	var commitmentH1, commitmentH2, commitmentH3 kzg.Digest

	g.Go(func() (err error) {
		var subPk kzg.ProvingKey
		subPk.G1 = make([]bn254.G1Affine, len(h1))
		for i := 0; i < len(h1); i++ {
			subPk.G1[i].Set(&kzgPk.G1[i*t+rh1])
		}
		commitmentH1, err = kzg.Commit(h1, subPk)
		return
	})

	g.Go(func() (err error) {
		var subPk kzg.ProvingKey
		subPk.G1 = make([]bn254.G1Affine, len(h2))
		for i := 0; i < len(h1); i++ {
			subPk.G1[i].Set(&kzgPk.G1[i*t+rh2])
		}
		commitmentH2, err = kzg.Commit(h2, subPk)
		return
	})

	g.Go(func() (err error) {
		var subPk kzg.ProvingKey
		subPk.G1 = make([]bn254.G1Affine, len(h3))
		for i := 0; i < len(h1); i++ {
			subPk.G1[i].Set(&kzgPk.G1[i*t+rh3])
		}
		commitmentH3, err = kzg.Commit(h3, subPk)
		return
	})

	err := g.Wait()

	s.proof.HEntangled.Add(&commitmentH1, &commitmentH2).
		Add(&s.proof.HEntangled, &commitmentH3)

	return err
}

// divideByXMinusOne
// The input must be in LagrangeCoset.
// The result is in Canonical Regular. (in place using a)
func divideByXMinusOne(a *iop.Polynomial, domains [2]*fft.Domain) (*iop.Polynomial, error) {

	// check that the basis is LagrangeCoset
	if a.Basis != iop.LagrangeCoset || a.Layout != iop.BitReverse {
		return nil, errors.New("invalid form")
	}

	// prepare the evaluations of x^n-1 on the big domain's coset
	xnMinusOneInverseLagrangeCoset := evaluateXnMinusOneDomainBigCoset(domains)
	rho := int(domains[1].Cardinality / domains[0].Cardinality)

	r := a.Coefficients()
	n := uint64(len(r))
	nn := uint64(64 - bits.TrailingZeros64(n))

	utils.Parallelize(len(r), func(start, end int) {
		for i := start; i < end; i++ {
			iRev := bits.Reverse64(uint64(i)) >> nn
			r[i].Mul(&r[i], &xnMinusOneInverseLagrangeCoset[int(iRev)%rho])
		}
	})

	// since a is in bit reverse order, ToRegular shouldn't do anything
	a.ToCanonical(domains[1]).ToRegular()

	return a, nil

}

// evaluateXnMinusOneDomainBigCoset evaluates Xᵐ-1 on DomainBig coset
func evaluateXnMinusOneDomainBigCoset(domains [2]*fft.Domain) []fr.Element {

	rho := domains[1].Cardinality / domains[0].Cardinality

	res := make([]fr.Element, rho)

	expo := big.NewInt(int64(domains[0].Cardinality))
	res[0].Exp(domains[1].FrMultiplicativeGen, expo)

	var t fr.Element
	t.Exp(domains[1].Generator, big.NewInt(int64(domains[0].Cardinality)))

	one := fr.One()

	for i := 1; i < int(rho); i++ {
		res[i].Mul(&res[i-1], &t)
		res[i-1].Sub(&res[i-1], &one)
	}
	res[len(res)-1].Sub(&res[len(res)-1], &one)

	res = fr.BatchInvert(res)

	return res
}

// innerComputeLinearizedPoly computes the linearized polynomial in canonical basis.
// The purpose is to commit and open all in one ql, qr, qm, qo, qk.
// * lZeta, rZeta, oZeta are the evaluation of l, r, o at zeta
// * z is the permutation polynomial, zu is Z(μX), the shifted version of Z
// * pk is the proving key: the linearized polynomial is a linear combination of ql, qr, qm, qo, qk.
//
// The Linearized polynomial is:
//
// α²*L₁(ζ)*Z(X)
// + α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*(β*s3(X))*Z(μζ) - Z(X)*(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ))
// + l(ζ)*Ql(X) + l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X) + ∑ᵢQcp_(ζ)Pi_(X)
// - Z_{H}(ζ)*((H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
func (s *instance) innerComputeLinearizedPoly(lZeta, rZeta, oZeta, alpha, beta, gamma, zeta, zu fr.Element, qcpZeta, blindedZCanonical []fr.Element, pi2Canonical [][]fr.Element, pk *ProvingKey) []fr.Element {

	// TODO @gbotrel rename

	// l(ζ)r(ζ)
	var rl fr.Element
	rl.Mul(&rZeta, &lZeta)

	// s1 =  α*(l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)
	// s2 = -α*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	// the linearised polynomial is
	// α²*L₁(ζ)*Z(X) +
	// s1*s3(X)+s2*Z(X) + l(ζ)*Ql(X) +
	// l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X) + ∑ᵢQcp_(ζ)Pi_(X) -
	// Z_{H}(ζ)*((H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
	var s1, s2 fr.Element
	chS1 := make(chan struct{}, 1)
	go func() {
		s1 = s.trace.S1.Evaluate(zeta)                       // s1(ζ)
		s1.Mul(&s1, &beta).Add(&s1, &lZeta).Add(&s1, &gamma) // (l(ζ)+β*s1(ζ)+γ)
		close(chS1)
	}()

	tmp := s.trace.S2.Evaluate(zeta)                         // s2(ζ)
	tmp.Mul(&tmp, &beta).Add(&tmp, &rZeta).Add(&tmp, &gamma) // (r(ζ)+β*s2(ζ)+γ)
	<-chS1
	s1.Mul(&s1, &tmp).Mul(&s1, &zu).Mul(&s1, &beta).Mul(&s1, &alpha) // (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*β*Z(μζ)*α

	var uzeta, uuzeta fr.Element
	uzeta.Mul(&zeta, &pk.Vk.CosetShift)
	uuzeta.Mul(&uzeta, &pk.Vk.CosetShift)

	s2.Mul(&beta, &zeta).Add(&s2, &lZeta).Add(&s2, &gamma)      // (l(ζ)+β*ζ+γ)
	tmp.Mul(&beta, &uzeta).Add(&tmp, &rZeta).Add(&tmp, &gamma)  // (r(ζ)+β*u*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)
	tmp.Mul(&beta, &uuzeta).Add(&tmp, &oZeta).Add(&tmp, &gamma) // (o(ζ)+β*u²*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	s2.Neg(&s2).Mul(&s2, &alpha)

	// Z_h(ζ), ζⁿ⁺², L₁(ζ)*α²*Z
	var zhZeta, zetaNPlusTwo, alphaSquareLagrangeOne, one, den, frNbElmt fr.Element
	one.SetOne()
	nbElmt := int64(s.domain0.Cardinality)
	alphaSquareLagrangeOne.Set(&zeta).Exp(alphaSquareLagrangeOne, big.NewInt(nbElmt)) // ζⁿ
	zetaNPlusTwo.Mul(&alphaSquareLagrangeOne, &zeta).Mul(&zetaNPlusTwo, &zeta)        // ζⁿ⁺²
	alphaSquareLagrangeOne.Sub(&alphaSquareLagrangeOne, &one)                         // ζⁿ - 1
	zhZeta.Set(&alphaSquareLagrangeOne)                                               // Z_h(ζ) = ζⁿ - 1
	frNbElmt.SetUint64(uint64(nbElmt))
	den.Sub(&zeta, &one).Inverse(&den)                         // 1/(ζ-1)
	alphaSquareLagrangeOne.Mul(&alphaSquareLagrangeOne, &den). // L₁ = (ζⁿ - 1)/(ζ-1)
									Mul(&alphaSquareLagrangeOne, &alpha).
									Mul(&alphaSquareLagrangeOne, &alpha).
									Mul(&alphaSquareLagrangeOne, &s.domain0.CardinalityInv) // α²*L₁(ζ)

	s3canonical := s.trace.S3.Coefficients()

	s.trace.Qk.ToCanonical(s.domain0).ToRegular()

	// the hi are all of the same length
	h1 := s.h1()
	h2 := s.h2()
	h3 := s.h3()

	// at this stage we have
	// s1 =  α*(l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)
	// s2 = -α*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	utils.Parallelize(len(blindedZCanonical), func(start, end int) {

		cql := s.trace.Ql.Coefficients()
		cqr := s.trace.Qr.Coefficients()
		cqm := s.trace.Qm.Coefficients()
		cqo := s.trace.Qo.Coefficients()
		cqk := s.trace.Qk.Coefficients()

		var t, t0, t1 fr.Element

		for i := start; i < end; i++ {
			t.Mul(&blindedZCanonical[i], &s2) // -Z(X)*α*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
			if i < len(s3canonical) {
				t0.Mul(&s3canonical[i], &s1) // α*(l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)*β*s3(X)
				t.Add(&t, &t0)
			}
			if i < len(cqm) {
				t1.Mul(&cqm[i], &rl)     // l(ζ)r(ζ)*Qm(X)
				t.Add(&t, &t1)           // linPol += l(ζ)r(ζ)*Qm(X)
				t0.Mul(&cql[i], &lZeta)  // l(ζ)Q_l(X)
				t.Add(&t, &t0)           // linPol += l(ζ)*Ql(X)
				t0.Mul(&cqr[i], &rZeta)  //r(ζ)*Qr(X)
				t.Add(&t, &t0)           // linPol += r(ζ)*Qr(X)
				t0.Mul(&cqo[i], &oZeta)  // o(ζ)*Qo(X)
				t.Add(&t, &t0)           // linPol += o(ζ)*Qo(X)
				t.Add(&t, &cqk[i])       // linPol += Qk(X)
				for j := range qcpZeta { // linPol += ∑ᵢQcp_(ζ)Pi_(X)
					t0.Mul(&pi2Canonical[j][i], &qcpZeta[j])
					t.Add(&t, &t0)
				}
			}

			t0.Mul(&blindedZCanonical[i], &alphaSquareLagrangeOne) // α²L₁(ζ)Z(X)
			blindedZCanonical[i].Add(&t, &t0)                      // linPol += α²L₁(ζ)Z(X)

			if i < len(h1) {
				t.Mul(&h3[i], &zetaNPlusTwo).
					Add(&t, &h2[i]).
					Mul(&t, &zetaNPlusTwo).
					Add(&t, &h1[i])
				t.Mul(&t, &zhZeta)
				blindedZCanonical[i].Sub(&blindedZCanonical[i], &t) // linPol -= Z_h(ζ)*(H₀(X) + ζᵐ⁺²*H₁(X) + ζ²⁽ᵐ⁺²⁾*H₂(X))
			}

		}
	})
	return blindedZCanonical
}

var errContextDone = errors.New("context done")
