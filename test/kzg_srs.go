/*
Copyright © 2021 ConsenSys Software Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"crypto/rand"
	"math/big"
	"sync"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/kzg"
	"github.com/consensys/gnark/constraint"
	"github.com/consensys/gnark/internal/utils"

	kzg_bls12377 "github.com/consensys/gnark-crypto/ecc/bls12-377/kzg"
	kzg_bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381/kzg"
	kzg_bls24315 "github.com/consensys/gnark-crypto/ecc/bls24-315/kzg"
	kzg_bls24317 "github.com/consensys/gnark-crypto/ecc/bls24-317/kzg"
	kzg_bn254 "github.com/consensys/gnark-crypto/ecc/bn254/kzg"
	kzg_bw6633 "github.com/consensys/gnark-crypto/ecc/bw6-633/kzg"
	kzg_bw6761 "github.com/consensys/gnark-crypto/ecc/bw6-761/kzg"
)

const srsCachedSize = (1 << 14) + 3

// NewKZGSRS uses ccs nb variables and nb constraints to initialize a kzg srs
// for sizes < 2¹⁵, returns a pre-computed cached SRS
//
// /!\ warning /!\: this method is here for convenience only: in production, a SRS generated through MPC should be used.
func NewKZGSRS(ccs constraint.ConstraintSystem) (kzg.SRS, error) {

	kzgSize := getSizeSRS(ccs)

	if kzgSize <= srsCachedSize {
		return getCachedSRS(ccs)
	}

	curveID := utils.FieldToCurve(ccs.Field())
	alpha, err := rand.Int(rand.Reader, curveID.ScalarField())
	if err != nil {
		return nil, err
	}
	return newKZGSRS(utils.FieldToCurve(ccs.Field()), kzgSize, alpha)
}

// NewDummyKZGSRS generates an SRS with τ = 1 for fast generation
//
// /!\ method to call for benchmarking only
func NewDummyKZGSRS(ccs constraint.ConstraintSystem) (kzg.SRS, error) {

	kzgSize := getSizeSRS(ccs)

	return newKZGSRS(utils.FieldToCurve(ccs.Field()), kzgSize, big.NewInt(1))
}

func getSizeSRS(ccs constraint.ConstraintSystem) uint64 {

	nbConstraints := ccs.GetNbConstraints()
	sizeSystem := nbConstraints + ccs.GetNbPublicVariables()
	kzgSize := ecc.NextPowerOfTwo(uint64(sizeSystem)) + 3 // +3 due to the blinding
	return kzgSize

}

var srsCache map[ecc.ID]kzg.SRS
var lock sync.Mutex

func init() {
	srsCache = make(map[ecc.ID]kzg.SRS)
}
func getCachedSRS(ccs constraint.ConstraintSystem) (kzg.SRS, error) {
	lock.Lock()
	defer lock.Unlock()

	curveID := utils.FieldToCurve(ccs.Field())

	if srs, ok := srsCache[curveID]; ok {
		return srs, nil
	}

	alpha, err := rand.Int(rand.Reader, curveID.ScalarField())
	if err != nil {
		return nil, err
	}
	srs, err := newKZGSRS(curveID, srsCachedSize, alpha)
	if err != nil {
		return nil, err
	}
	srsCache[curveID] = srs
	return srs, nil
}

func newKZGSRS(curve ecc.ID, kzgSize uint64, alpha *big.Int) (kzg.SRS, error) {

	switch curve {
	case ecc.BN254:
		return kzg_bn254.NewSRS(kzgSize, alpha)
	case ecc.BLS12_381:
		return kzg_bls12381.NewSRS(kzgSize, alpha)
	case ecc.BLS12_377:
		return kzg_bls12377.NewSRS(kzgSize, alpha)
	case ecc.BW6_761:
		return kzg_bw6761.NewSRS(kzgSize, alpha)
	case ecc.BLS24_317:
		return kzg_bls24317.NewSRS(kzgSize, alpha)
	case ecc.BLS24_315:
		return kzg_bls24315.NewSRS(kzgSize, alpha)
	case ecc.BW6_633:
		return kzg_bw6633.NewSRS(kzgSize, alpha)
	default:
		panic("unrecognized R1CS curve type")
	}
}
