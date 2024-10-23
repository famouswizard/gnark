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

package mpcsetup

import (
	curve "github.com/consensys/gnark-crypto/ecc/bn254"
	"io"
)

func appendRefs[T any](s []interface{}, v []T) []interface{} {
	for i := range v {
		s = append(s, &v[i])
	}
	return s
}

// proofRefsSlice produces a slice consisting of references to all proof sub-elements
// prepended by the size parameter, to be used in WriteTo and ReadFrom functions
func (p *Phase1) proofRefsSlice() []interface{} {
	return []interface{}{
		&p.proofs.Tau.contributionCommitment,
		&p.proofs.Tau.contributionPok,
		&p.proofs.Alpha.contributionCommitment,
		&p.proofs.Alpha.contributionPok,
		&p.proofs.Beta.contributionCommitment,
		&p.proofs.Beta.contributionPok,
	}
}

// WriteTo implements io.WriterTo
// It does not write the Challenge from the previous contribution
func (p *Phase1) WriteTo(writer io.Writer) (n int64, err error) {

	if n, err = p.parameters.WriteTo(writer); err != nil {
		return
	}

	enc := curve.NewEncoder(writer)
	for _, v := range p.proofRefsSlice() {
		if err = enc.Encode(v); err != nil {
			return n + enc.BytesWritten(), err
		}
	}
	return n + enc.BytesWritten(), nil
}

// ReadFrom implements io.ReaderFrom
// It does not read the Challenge from the previous contribution
func (p *Phase1) ReadFrom(reader io.Reader) (n int64, err error) {

	if n, err = p.parameters.ReadFrom(reader); err != nil {
		return
	}

	dec := curve.NewDecoder(reader)
	for _, v := range p.proofRefsSlice() { // we've already decoded N
		if err = dec.Decode(v); err != nil {
			return n + dec.BytesRead(), err
		}
	}
	return n + dec.BytesRead(), nil
}

// WriteTo implements io.WriterTo
func (phase2 *Phase2) WriteTo(writer io.Writer) (int64, error) {
	n, err := phase2.writeTo(writer)
	if err != nil {
		return n, err
	}
	nBytes, err := writer.Write(phase2.Hash)
	return int64(nBytes) + n, err
}

func (c *Phase2) writeTo(writer io.Writer) (int64, error) {
	enc := curve.NewEncoder(writer)
	toEncode := []interface{}{
		&c.PublicKey.SG,
		&c.PublicKey.SXG,
		&c.PublicKey.XR,
		&c.Parameters.G1.Delta,
		c.Parameters.G1.PKK,
		c.Parameters.G1.Z,
		&c.Parameters.G2.Delta,
	}

	for _, v := range toEncode {
		if err := enc.Encode(v); err != nil {
			return enc.BytesWritten(), err
		}
	}

	return enc.BytesWritten(), nil
}

// ReadFrom implements io.ReaderFrom
func (c *Phase2) ReadFrom(reader io.Reader) (int64, error) {
	dec := curve.NewDecoder(reader)
	toEncode := []interface{}{
		&c.PublicKey.SG,
		&c.PublicKey.SXG,
		&c.PublicKey.XR,
		&c.Parameters.G1.Delta,
		&c.Parameters.G1.PKK,
		&c.Parameters.G1.Z,
		&c.Parameters.G2.Delta,
	}

	for _, v := range toEncode {
		if err := dec.Decode(v); err != nil {
			return dec.BytesRead(), err
		}
	}

	c.Hash = make([]byte, 32)
	n, err := reader.Read(c.Hash)
	return int64(n) + dec.BytesRead(), err

}

// WriteTo implements io.WriterTo
func (c *Phase2Evaluations) WriteTo(writer io.Writer) (int64, error) {
	enc := curve.NewEncoder(writer)
	toEncode := []interface{}{
		c.G1.A,
		c.G1.B,
		c.G2.B,
	}

	for _, v := range toEncode {
		if err := enc.Encode(v); err != nil {
			return enc.BytesWritten(), err
		}
	}

	return enc.BytesWritten(), nil
}

// ReadFrom implements io.ReaderFrom
func (c *Phase2Evaluations) ReadFrom(reader io.Reader) (int64, error) {
	dec := curve.NewDecoder(reader)
	toEncode := []interface{}{
		&c.G1.A,
		&c.G1.B,
		&c.G2.B,
	}

	for _, v := range toEncode {
		if err := dec.Decode(v); err != nil {
			return dec.BytesRead(), err
		}
	}

	return dec.BytesRead(), nil
}

// refsSlice produces a slice consisting of references to all sub-elements
// prepended by the size parameter, to be used in WriteTo and ReadFrom functions
func (c *SrsCommons) refsSlice() []interface{} {
	N := len(c.G2.Tau)
	estimatedNbElems := 5*N - 1
	// size N                                                                    1
	// 𝔾₂ representation for β                                                   1
	// [τⁱ]₁  for 1 ≤ i ≤ 2N-2                                                2N-2
	// [τⁱ]₂  for 1 ≤ i ≤ N-1                                                  N-1
	// [ατⁱ]₁ for 0 ≤ i ≤ N-1                                                  N
	// [βτⁱ]₁ for 0 ≤ i ≤ N-1                                                  N
	refs := make([]interface{}, 1, estimatedNbElems)
	refs[0] = N

	refs = appendRefs(refs, c.G1.Tau[1:])
	refs = appendRefs(refs, c.G2.Tau[1:])
	refs = appendRefs(refs, c.G1.BetaTau)
	refs = appendRefs(refs, c.G1.AlphaTau)

	if len(refs) != estimatedNbElems {
		panic("incorrect length estimate")
	}

	return refs
}

func (c *SrsCommons) WriteTo(writer io.Writer) (int64, error) {
	enc := curve.NewEncoder(writer)
	for _, v := range c.refsSlice() {
		if err := enc.Encode(v); err != nil {
			return enc.BytesWritten(), err
		}
	}
	return enc.BytesWritten(), nil
}

// ReadFrom implements io.ReaderFrom
func (c *SrsCommons) ReadFrom(reader io.Reader) (n int64, err error) {
	var N uint64
	dec := curve.NewDecoder(reader)
	if err = dec.Decode(&N); err != nil {
		return dec.BytesRead(), err
	}

	c.setZero(N)

	for _, v := range c.refsSlice()[1:] { // we've already decoded N
		if err = dec.Decode(v); err != nil {
			return dec.BytesRead(), err
		}
	}
	return dec.BytesRead(), nil
}
