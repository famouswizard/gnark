// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package cs

import (
	"fmt"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/fxamacker/cbor/v2"
	"io"
	"math/big"

	"github.com/consensys/gnark/internal/backend/compiled"
	"github.com/consensys/gnark/internal/backend/ioutils"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
)

// SparseR1CS represents a Plonk like circuit
type SparseR1CS struct {
	compiled.SparseR1CS

	// Coefficients in the constraints
	Coefficients []fr.Element // list of unique coefficients.
}

// NewSparseR1CS returns a new SparseR1CS and sets r1cs.Coefficient (fr.Element) from provided big.Int values
func NewSparseR1CS(r1cs compiled.SparseR1CS, coefficients []big.Int) *SparseR1CS {
	cs := SparseR1CS{
		r1cs,
		make([]fr.Element, len(coefficients)),
	}
	for i := 0; i < len(coefficients); i++ {
		cs.Coefficients[i].SetBigInt(&coefficients[i])
	}
	return &cs
}

// FrSize return fr.Limbs * 8, size in byte of a fr element
func (cs *SparseR1CS) FrSize() int {
	return fr.Limbs * 8
}

// GetNbCoefficients return the number of unique coefficients needed in the R1CS
func (cs *SparseR1CS) GetNbCoefficients() int {
	return len(cs.Coefficients)
}

// CurveID returns curve ID as defined in gnark-crypto (ecc.BN254)
func (cs *SparseR1CS) CurveID() ecc.ID {
	return ecc.BN254
}

// WriteTo encodes SparseR1CS into provided io.Writer using cbor
func (cs *SparseR1CS) WriteTo(w io.Writer) (int64, error) {
	_w := ioutils.WriterCounter{W: w} // wraps writer to count the bytes written
	encoder := cbor.NewEncoder(&_w)

	// encode our object
	err := encoder.Encode(cs)
	return _w.N, err
}

// ReadFrom attempts to decode SparseR1CS from io.Reader using cbor
func (cs *SparseR1CS) ReadFrom(r io.Reader) (int64, error) {
	dm, err := cbor.DecOptions{MaxArrayElements: 134217728}.DecMode()
	if err != nil {
		return 0, err
	}
	decoder := dm.NewDecoder(r)
	err = decoder.Decode(cs)
	return int64(decoder.NumBytesRead()), err
}

// find unsolved variable
// returns 0 if the variable to solve is L, 1 if it's R, 2 if it's O
func findUnsolvedVariable(c compiled.SparseR1C, wireInstantiated []bool) int {
	if c.L.CoeffID() != 0 && !wireInstantiated[c.L.VariableID()] {
		return 0
	}
	if c.M[0].CoeffID() != 0 && !wireInstantiated[c.M[0].VariableID()] {
		// M[0] corresponds to L by default
		return 0
	}
	if c.R.CoeffID() != 0 && !wireInstantiated[c.R.VariableID()] {
		return 1
	}
	if c.M[1].CoeffID() != 0 && !wireInstantiated[c.M[1].VariableID()] {
		// M[1] corresponds to R by default
		return 1
	}
	// TODO panic if wire is already instantiated
	// only O remains
	return 2
}

// computeTerm computes coef*variable
func (cs *SparseR1CS) computeTerm(t compiled.Term, solution []fr.Element) fr.Element {
	cID, vID, _ := t.Unpack()
	switch cID {
	case compiled.CoeffIdZero:
		return fr.Element{}
	case compiled.CoeffIdOne:
		return solution[vID]
	case compiled.CoeffIdTwo:
		var res fr.Element
		res.Double(&solution[vID])
		return res
	case compiled.CoeffIdMinusOne:
		var res fr.Element
		res.Neg(&solution[vID])
		return res
	default:
		var res fr.Element
		res.Mul(&cs.Coefficients[cID], &solution[vID])
		return res
	}
}

// solveConstraint solves c with the help of the slices wireInstantiated
// and solution. Those are used to find which variable remains to be solved,
// and the way of solving it (binary or single value). Once the variable(s)
// is solved, solution and wireInstantiated are updated.
func (cs *SparseR1CS) solveConstraint(c compiled.SparseR1C, wireInstantiated []bool, solution, coefficientsNegInv []fr.Element) {

	switch c.Solver {
	case compiled.IsZero:
		// inital constraint is in the form a * m == 0
		// it was transform in a plonk constraint like so:
		// L = constantTerm(a) * m
		// M[0] = m
		// M[1] = a - constantTerm(a)

		// we want to reconstruct a, and compute m = 1 - a^(q-1)
		vID := c.L.VariableID()

		// sanity checks
		lro := findUnsolvedVariable(c, wireInstantiated)
		if lro != 0 || !wireInstantiated[c.M[1].VariableID()] || c.M[0].VariableID() != vID {
			panic("sanity check for plonk.IsZero failed")
		}

		// reconstruct a
		a := cs.Coefficients[c.L.CoeffID()]
		a.Add(&a, &solution[c.M[1].VariableID()])

		// compute a ^ (q-1)
		// q - 1
		var eOne big.Int
		eOne.SetUint64(1)
		eOne.Sub(fr.Modulus(), &eOne)

		one := fr.One()
		solution[vID].Exp(a, &eOne)
		// m = 1 - a ^ (q-1)
		solution[vID].Sub(&one, &solution[vID])
		wireInstantiated[vID] = true

	case compiled.SingleOutput:

		lro := findUnsolvedVariable(c, wireInstantiated)
		if lro == 0 { // we solve for L: u1L+u2R+u3LR+u4O+k=0 => L(u1+u3R)+u2R+u4O+k = 0

			var u1, u2, u3, den, num, v1, v2 fr.Element
			u3.Mul(&cs.Coefficients[c.M[0].CoeffID()], &cs.Coefficients[c.M[1].CoeffID()])
			u1.Set(&cs.Coefficients[c.L.CoeffID()])
			u2.Set(&cs.Coefficients[c.R.CoeffID()])
			den.Mul(&u3, &solution[c.R.VariableID()]).Add(&den, &u1)

			v1 = cs.computeTerm(c.R, solution)
			v2 = cs.computeTerm(c.O, solution)
			num.Add(&v1, &v2).Add(&num, &cs.Coefficients[c.K])

			// TODO find a way to do lazy div (/ batch inversion)
			solution[c.L.VariableID()].Div(&num, &den).Neg(&solution[c.L.VariableID()])
			wireInstantiated[c.L.VariableID()] = true

		} else if lro == 1 { // we solve for R: u1L+u2R+u3LR+u4O+k=0 => R(u2+u3L)+u1L+u4O+k = 0

			var u1, u2, u3, den, num, v1, v2 fr.Element
			u3.Mul(&cs.Coefficients[c.M[0].VariableID()], &cs.Coefficients[c.M[1].VariableID()])
			u1.Set(&cs.Coefficients[c.L.CoeffID()])
			u2.Set(&cs.Coefficients[c.R.CoeffID()])
			den.Mul(&u3, &solution[c.L.VariableID()]).Add(&den, &u2)

			v1 = cs.computeTerm(c.L, solution)
			v2 = cs.computeTerm(c.O, solution)
			num.Add(&v1, &v2).Add(&num, &cs.Coefficients[c.K])

			// TODO find a way to do lazy div (/ batch inversion)
			solution[c.L.VariableID()].Div(&num, &den).Neg(&solution[c.L.VariableID()])
			wireInstantiated[c.L.VariableID()] = true

		} else { // O we solve for O
			var o fr.Element
			cID, vID, _ := c.O.Unpack()

			l := cs.computeTerm(c.L, solution)
			r := cs.computeTerm(c.R, solution)
			m0 := cs.computeTerm(c.M[0], solution)
			m1 := cs.computeTerm(c.M[1], solution)

			// o = - ((m0 * m1) + l + r + c.K) / c.O
			o.Mul(&m0, &m1).Add(&o, &l).Add(&o, &r).Add(&o, &cs.Coefficients[c.K])
			o.Mul(&o, &coefficientsNegInv[cID])

			solution[vID] = o
			wireInstantiated[vID] = true
		}

	case compiled.BinaryDec:
		// 2*L + R + O = 0, computed as a = c/2, b = c%2
		var bo, bl, br, two big.Int
		o := cs.computeTerm(c.O, solution)
		o.Neg(&o)
		o.ToBigIntRegular(&bo)
		two.SetInt64(2)
		br.Mod(&bo, &two)
		bl.Rsh(&bo, 1)
		solution[c.L.VariableID()].SetBigInt(&bl)
		solution[c.R.VariableID()].SetBigInt(&br)
		wireInstantiated[c.L.VariableID()] = true
		wireInstantiated[c.R.VariableID()] = true

	default:
		panic("unimplemented solving method")
	}

}

// IsSolved returns nil if given witness solves the R1CS and error otherwise
// this method wraps r1cs.Solve() and allocates r1cs.Solve() inputs
func (cs *SparseR1CS) IsSolved(witness []fr.Element) error {
	_, err := cs.Solve(witness)
	return err
}

// checkConstraint verifies that the constraint holds
func (cs *SparseR1CS) checkConstraint(c compiled.SparseR1C, solution []fr.Element) error {
	l := cs.computeTerm(c.L, solution)
	r := cs.computeTerm(c.R, solution)
	m0 := cs.computeTerm(c.M[0], solution)
	m1 := cs.computeTerm(c.M[1], solution)
	o := cs.computeTerm(c.O, solution)

	// l + r + (m0 * m1) + o + c.K == 0
	var t fr.Element
	t.Mul(&m0, &m1).Add(&t, &l).Add(&t, &r).Add(&t, &o).Add(&t, &cs.Coefficients[c.K])
	if !t.IsZero() {
		return ErrUnsatisfiedConstraint
	}
	return nil

}

// Solve sets all the wires.
// wireValues =  [publicInputs | secretInputs | internalVariables ]
// witness: contains the input variables
// it returns the full slice of wires
func (cs *SparseR1CS) Solve(witness []fr.Element) (solution []fr.Element, err error) {

	expectedWitnessSize := int(cs.NbPublicVariables + cs.NbSecretVariables)
	if len(witness) != expectedWitnessSize {
		return nil, fmt.Errorf(
			"invalid witness size, got %d, expected %d = %d (public) + %d (secret)",
			len(witness),
			expectedWitnessSize,
			cs.NbPublicVariables,
			cs.NbSecretVariables,
		)
	}

	// set the slices holding the solution and monitoring which variables have been solved
	nbVariables := cs.NbInternalVariables + cs.NbSecretVariables + cs.NbPublicVariables
	solution = make([]fr.Element, nbVariables)
	wireInstantiated := make([]bool, nbVariables)

	// solution = [publicInputs | secretInputs | internalVariables ] -> we fill publicInputs | secretInputs
	copy(solution, witness)
	for i := 0; i < len(witness); i++ {
		wireInstantiated[i] = true
	}

	// defer log printing once all wireValues are computed
	defer cs.printLogs(solution, wireInstantiated)

	coefficientsNegInv := fr.BatchInvert(cs.Coefficients)
	for i := 0; i < len(coefficientsNegInv); i++ {
		coefficientsNegInv[i].Neg(&coefficientsNegInv[i])
	}

	// loop through the constraints to solve the variables
	for i := 0; i < len(cs.Constraints); i++ {
		cs.solveConstraint(cs.Constraints[i], wireInstantiated, solution, coefficientsNegInv)
		err = cs.checkConstraint(cs.Constraints[i], solution)
		if err != nil {
			fmt.Printf("%d-th constraint\n", i)
			return solution, err
		}
	}

	// loop through the assertions and check consistency
	for i := 0; i < len(cs.Assertions); i++ {
		err = cs.checkConstraint(cs.Assertions[i], solution)
		if err != nil {
			return solution, err
		}
	}

	return solution, nil

}

func logValue(entry compiled.LogEntry, wireValues []fr.Element, wireInstantiated []bool) string {
	var toResolve []interface{}
	for j := 0; j < len(entry.ToResolve); j++ {
		wireID := entry.ToResolve[j]
		if !wireInstantiated[wireID] {
			panic("wire values was not instantiated")
		}
		toResolve = append(toResolve, wireValues[wireID].String())
	}
	return fmt.Sprintf(entry.Format, toResolve...)
}

func (cs *SparseR1CS) printLogs(wireValues []fr.Element, wireInstantiated []bool) {

	// for each log, resolve the wire values and print the log to stdout
	for i := 0; i < len(cs.Logs); i++ {
		fmt.Print(logValue(cs.Logs[i], wireValues, wireInstantiated))
	}
}