package plonk_test

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/consensys/gnark"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/kzg"
	"github.com/consensys/gnark/backend/plonk"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/scs"
	"github.com/consensys/gnark/test"
	"github.com/stretchr/testify/require"
)

func TestProver(t *testing.T) {

	for _, curve := range getCurves() {
		t.Run(curve.String(), func(t *testing.T) {
			var b1, b2 bytes.Buffer
			assert := require.New(t)

			ccs, _solution := referenceCircuit(curve)
			srs, err := test.NewKZGSRS(ccs)
			assert.NoError(err)
			fullWitness, err := frontend.NewWitness(_solution, curve.ScalarField())
			assert.NoError(err)

			publicWitness, err := fullWitness.Public()
			assert.NoError(err)

			pk, vk, err := plonk.Setup(ccs, srs)
			assert.NoError(err)

			// write the PK to ensure it is not mutated
			_, err = pk.WriteTo(&b1)
			assert.NoError(err)

			proof, err := plonk.Prove(ccs, pk, fullWitness)
			assert.NoError(err)

			// check pk
			_, err = pk.WriteTo(&b2)
			assert.NoError(err)

			assert.True(bytes.Equal(b1.Bytes(), b2.Bytes()), "plonk prover mutated the proving key")

			err = plonk.Verify(proof, vk, publicWitness)
			assert.NoError(err)

		})

	}
}

//--------------------//
//     benches		  //
//--------------------//

func BenchmarkSetup(b *testing.B) {

	// ccs, _ := referenceCircuit(curve)
	// srs, _ := test.NewKZGSRS(ccs)
	b.Run("dummy setup", func(b *testing.B) {
		for _, curve := range getCurves() {
			b.Run(curve.String(), func(b *testing.B) {
				ccs, _, srs := referenceCircuitDummySRS(curve)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _, _ = plonk.DummySetup(ccs, srs)
				}
			})
		}
	})
	b.Run("real setup", func(b *testing.B) {
		for _, curve := range getCurves() {
			b.Run(curve.String(), func(b *testing.B) {
				ccs, _ := referenceCircuit(curve)
				srs, _ := test.NewKZGSRS(ccs)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _, _ = plonk.Setup(ccs, srs)
				}
			})
		}
	})
}

// /!\ the benchmarks must be the same
func BenchmarkProver(b *testing.B) {

	b.Run("dummy setup", func(b *testing.B) {
		for _, curve := range getCurves() {
			b.Run(curve.String(), func(b *testing.B) {
				ccs, _solution, srs := referenceCircuitDummySRS(curve)
				fullWitness, err := frontend.NewWitness(_solution, curve.ScalarField())
				if err != nil {
					b.Fatal(err)
				}
				pk, _, err := plonk.DummySetup(ccs, srs)
				if err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _ = plonk.Prove(ccs, pk, fullWitness)
				}
			})
		}
	})
	b.Run("real setup", func(b *testing.B) {
		for _, curve := range getCurves() {
			b.Run(curve.String(), func(b *testing.B) {
				ccs, _solution := referenceCircuit(curve)
				srs, _ := test.NewKZGSRS(ccs)
				fullWitness, err := frontend.NewWitness(_solution, curve.ScalarField())
				if err != nil {
					b.Fatal(err)
				}
				pk, _, err := plonk.Setup(ccs, srs)
				if err != nil {
					b.Fatal(err)
				}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _ = plonk.Prove(ccs, pk, fullWitness)
				}
			})
		}
	})
}

func BenchmarkVerifier(b *testing.B) {
	for _, curve := range getCurves() {
		b.Run(curve.String(), func(b *testing.B) {
			ccs, _solution, srs := referenceCircuitDummySRS(curve)
			fullWitness, err := frontend.NewWitness(_solution, curve.ScalarField())
			if err != nil {
				b.Fatal(err)
			}
			publicWitness, err := fullWitness.Public()
			if err != nil {
				b.Fatal(err)
			}

			pk, vk, err := plonk.Setup(ccs, srs)
			if err != nil {
				b.Fatal(err)
			}
			proof, err := plonk.Prove(ccs, pk, fullWitness)
			if err != nil {
				panic(err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = plonk.Verify(proof, vk, publicWitness)
			}
		})
	}
}

type refCircuit struct {
	nbConstraints int
	X             frontend.Variable
	Y             frontend.Variable `gnark:",public"`
}

func (circuit *refCircuit) Define(api frontend.API) error {
	for i := 0; i < circuit.nbConstraints; i++ {
		circuit.X = api.Mul(circuit.X, circuit.X)
	}
	api.AssertIsEqual(circuit.X, circuit.Y)
	return nil
}

func referenceCircuitDummySRS(curve ecc.ID) (constraint.ConstraintSystem, frontend.Circuit, kzg.SRS) {
	ccs, _solution := referenceCircuit(curve)
	srs, err := test.NewDummyKZGSRS(ccs)
	if err != nil {
		panic(err)
	}
	return ccs, _solution, srs

}

func referenceCircuit(curve ecc.ID) (constraint.ConstraintSystem, frontend.Circuit) {
	const nbConstraints = 40000
	circuit := refCircuit{
		nbConstraints: nbConstraints,
	}
	ccs, err := frontend.Compile(curve.ScalarField(), scs.NewBuilder, &circuit)
	if err != nil {
		panic(err)
	}

	var good refCircuit
	good.X = 2

	// compute expected Y
	expectedY := new(big.Int).SetUint64(2)
	exp := big.NewInt(1)
	exp.Lsh(exp, nbConstraints)
	expectedY.Exp(expectedY, exp, curve.ScalarField())

	good.Y = expectedY
	return ccs, &good
}

func getCurves() []ecc.ID {
	if testing.Short() {
		return []ecc.ID{ecc.BN254}
	}
	return gnark.Curves()
}
