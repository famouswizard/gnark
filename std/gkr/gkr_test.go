package gkr

import (
	"encoding/json"
	"fmt"
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/sumcheck"
	"github.com/consensys/gnark/test"
	"github.com/stretchr/testify/assert"
	"os"
	"path/filepath"
	"testing"
)

func TestSingleIdentityGateTwoInstances(t *testing.T) {
	generateTestVerifier("./test_vectors/single_identity_gate_two_instances.json")(t)
}

func TestGkrVectors(t *testing.T) {

	testDirPath := "./test_vectors"
	dirEntries, err := os.ReadDir(testDirPath)
	if err != nil {
		t.Error(err)
	}
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {

			if filepath.Ext(dirEntry.Name()) == ".json" {
				path := filepath.Join(testDirPath, dirEntry.Name())
				noExt := dirEntry.Name()[:len(dirEntry.Name())-len(".json")]

				t.Run(noExt+"_verifier", generateTestVerifier(path))

			}
		}
	}
}

func generateTestVerifier(path string) func(t *testing.T) {
	return func(t *testing.T) {

		testCase, err := newTestCase(path)
		assert.NoError(t, err)

		input := testCase.InOutAssignment.At(testCase.Circuit.InputLayer()...)
		output := testCase.InOutAssignment.At(testCase.Circuit.OutputLayer()...)

		partialSumPolys, finalEvalProofs := separateProof(testCase.Proof)

		circuit := &GkrVerifierCircuit{
			Input:                make([][]frontend.Variable, len(input)),
			Output:               make([][]frontend.Variable, len(output)),
			ProofPartialSumPolys: hollow(partialSumPolys),
			ProofFinalEvalProofs: hollow(finalEvalProofs),
			Statement:            0,
			TestCaseName:         path,
		}

		fillWithBlanks(circuit.Input, len(input[0]))
		fillWithBlanks(circuit.Output, len(input[0]))

		assignment := &GkrVerifierCircuit{
			Input:                input,
			Output:               output,
			ProofPartialSumPolys: partialSumPolys,
			ProofFinalEvalProofs: finalEvalProofs,
			Statement:            0,
			TestCaseName:         path,
		}

		test.NewAssert(t).ProverSucceeded(circuit, assignment, test.WithBackends(backend.GROTH16), test.WithCurves(ecc.BN254))

		circuit.Statement = 1
		assignment.Statement = 1

		//test.NewAssert(t).ProverFailed(circuit, assignment)
	}
}

type GkrVerifierCircuit struct {
	Input                [][]frontend.Variable
	Output               [][]frontend.Variable     `gnark:",public"`
	ProofPartialSumPolys [][][][]frontend.Variable // TODO: Feed a proof in here directly, without all this separating and interleaving business
	ProofFinalEvalProofs [][][]frontend.Variable
	Statement            frontend.Variable
	TestCaseName         string
}

func (c *GkrVerifierCircuit) Define(api frontend.API) error {
	api.Println("heloooooo")
	var circuit Circuit
	var transcript sumcheck.ArithmeticTranscript
	//var proofRef Proof
	if testCase, err := newTestCase(c.TestCaseName); err == nil {
		circuit = testCase.Circuit
		transcript = testCase.Transcript
	} else {
		return err
	}

	proof := interleaveProof(c.ProofPartialSumPolys, c.ProofFinalEvalProofs)
	assignment := makeInOutAssignment(circuit, c.Input, c.Output)
	transcript.Update(api, c.Statement)

	return Verify(api, circuit, assignment, proof, transcript)
}

func interleaveProof(partialSumPolys [][][][]frontend.Variable, finalEvalProofs [][][]frontend.Variable) Proof {
	proof := make(Proof, len(partialSumPolys))
	if len(partialSumPolys) != len(finalEvalProofs) {
		panic("malformed proof")
	}

	for i := range proof {
		proof[i] = make([]sumcheck.Proof, len(partialSumPolys[i]))
		if len(partialSumPolys[i]) != len(finalEvalProofs[i]) {
			panic("malformed prof")
		}
		for j := range proof[i] {
			proof[i][j] = interleaveSumcheckProof(partialSumPolys[i][j], finalEvalProofs[i][j])
		}
	}
	return proof
}

func separateProof(proof Proof) (partialSumPolys [][][][]frontend.Variable, finalEvalProofs [][][]frontend.Variable) {
	partialSumPolys = make([][][][]frontend.Variable, len(proof))
	finalEvalProofs = make([][][]frontend.Variable, len(proof))

	for i, pI := range proof {
		partialSumPolys[i] = make([][][]frontend.Variable, len(pI))
		finalEvalProofs[i] = make([][]frontend.Variable, len(pI))
		for j, pIJ := range pI {
			partialSumPolys[i][j], finalEvalProofs[i][j] = separateSumcheckProof(pIJ)
		}
	}
	return
}

func (a WireAssignment) addLayerValuations(layer CircuitLayer, values [][]frontend.Variable) {
	for i := range layer {
		a[&layer[i]] = values[i]
	}
}

func makeInOutAssignment(c Circuit, inputValues [][]frontend.Variable, outputValues [][]frontend.Variable) WireAssignment {
	res := make(WireAssignment, len(inputValues)+len(outputValues))
	res.addLayerValuations(c[len(c)-1], inputValues)
	res.addLayerValuations(c[0], outputValues)
	return res
}

func fillWithBlanks(slice [][]frontend.Variable, size int) {
	for i := range slice {
		slice[i] = make([]frontend.Variable, size)
	}
}

func (a WireAssignment) At(w ...*Wire) [][]frontend.Variable {
	res := make([][]frontend.Variable, len(w))

	for i, wI := range w {
		res[i] = a[wI]
	}

	return res
}

type TestCase struct {
	Circuit         Circuit
	Transcript      sumcheck.ArithmeticTranscript
	Proof           Proof
	InOutAssignment WireAssignment
}
type TestCaseInfo struct {
	Hash    string          `json:"hash"`
	Circuit string          `json:"circuit"`
	Input   [][]interface{} `json:"input"`
	Output  [][]interface{} `json:"output"`
	Proof   PrintableProof  `json:"proof"`
}

type ParsedTestCase struct {
	InOutAssignment WireAssignment
	Proof           Proof
	Hash            HashMap
	Circuit         Circuit
}

var parsedTestCases = make(map[string]*ParsedTestCase)

func newTestCase(path string) (*TestCase, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)

	parsedCase, ok := parsedTestCases[path]
	if !ok {
		var bytes []byte
		parsedCase = &ParsedTestCase{}
		if bytes, err = os.ReadFile(path); err == nil {
			var info TestCaseInfo
			err = json.Unmarshal(bytes, &info)
			if err != nil {
				return nil, err
			}

			parsedCase.Circuit, err = getCircuit(filepath.Join(dir, info.Circuit))

			if parsedCase.Hash, err = getHash(filepath.Join(dir, info.Hash)); err != nil {
				return nil, err
			}

			parsedCase.Proof = unmarshalProof(info.Proof)

			parsedCase.InOutAssignment = make(WireAssignment)

			{
				i := len(parsedCase.Circuit) - 1

				if len(parsedCase.Circuit[i]) != len(info.Input) {
					return nil, fmt.Errorf("input layer not the same size as input vector")
				}

				for j := range parsedCase.Circuit[i] {
					wire := &parsedCase.Circuit[i][j]
					wireAssignment := sliceToVariableSlice(info.Input[j])
					parsedCase.InOutAssignment[wire] = wireAssignment
				}
			}

			if len(parsedCase.Circuit[0]) != len(info.Output) {
				return nil, fmt.Errorf("output layer not the same size as output vector")
			}
			for j := range parsedCase.Circuit[0] {
				wire := &parsedCase.Circuit[0][j]
				parsedCase.InOutAssignment[wire] = sliceToVariableSlice(info.Output[j])
			}

			parsedTestCases[path] = parsedCase
		} else {
			return nil, err
		}
	}

	return &TestCase{
		Circuit:         parsedCase.Circuit,
		Transcript:      &MapHashTranscript{hashMap: parsedCase.Hash},
		InOutAssignment: parsedCase.InOutAssignment,
		Proof:           parsedCase.Proof,
	}, nil
}

type WireInfo struct {
	Gate   string  `json:"gate"`
	Inputs [][]int `json:"inputs"`
}

type CircuitInfo [][]WireInfo

var circuitCache = make(map[string]Circuit)

func getCircuit(path string) (Circuit, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if circuit, ok := circuitCache[path]; ok {
		return circuit, nil
	}
	var bytes []byte
	if bytes, err = os.ReadFile(path); err == nil {
		var circuitInfo CircuitInfo
		if err = json.Unmarshal(bytes, &circuitInfo); err == nil {
			circuit := circuitInfo.toCircuit()
			circuitCache[path] = circuit
			return circuit, nil
		} else {
			return nil, err
		}
	} else {
		return nil, err
	}
}

func (c CircuitInfo) toCircuit() (circuit Circuit) {
	isOutput := make(map[*Wire]interface{})
	circuit = make(Circuit, len(c))
	for i := len(c) - 1; i >= 0; i-- {
		circuit[i] = make(CircuitLayer, len(c[i]))
		for j, wireInfo := range c[i] {
			circuit[i][j].Gate = gates[wireInfo.Gate]
			circuit[i][j].Inputs = make([]*Wire, len(wireInfo.Inputs))
			isOutput[&circuit[i][j]] = nil
			for k, inputCoord := range wireInfo.Inputs {
				if len(inputCoord) != 2 {
					panic("circuit wire has two coordinates")
				}
				input := &circuit[inputCoord[0]][inputCoord[1]]
				input.NumOutputs++
				circuit[i][j].Inputs[k] = input
				delete(isOutput, input)
			}
			if (i == len(c)-1) != (len(circuit[i][j].Inputs) == 0) {
				panic("wire is input if and only if in last layer")
			}
		}
	}

	for k := range isOutput {
		k.NumOutputs = 1
	}

	return
}

var gates map[string]Gate

func init() {
	gates = make(map[string]Gate)
	gates["identity"] = identityGate{}
	gates["mul"] = mulGate{}
	gates["mimc"] = mimcCipherGate{} //TODO: Add ark
}

type mulGate struct{}

func (g mulGate) Evaluate(api frontend.API, x ...frontend.Variable) frontend.Variable {
	if len(x) != 2 {
		panic("mul has fan-in 2")
	}
	return api.Mul(x[0], x[1])
}

func (g mulGate) Degree() int {
	return 2
}

type mimcCipherGate struct {
	ark frontend.Variable
}

func (m mimcCipherGate) Evaluate(api frontend.API, input ...frontend.Variable) frontend.Variable {
	if len(input) != 2 {
		panic("mimc has fan-in 2")
	}
	sum := api.Add(&input[0], &input[1], &m.ark)

	sumCubed := api.Mul(sum, sum, sum) // sum^3
	return api.Mul(sumCubed, sumCubed, sum)
}

func (m mimcCipherGate) Degree() int {
	return 7
}

type PrintableProof [][]PrintableSumcheckProof

func unmarshalProof(printable PrintableProof) (proof Proof) {
	proof = make(Proof, len(printable))
	for i := range printable {
		proof[i] = make([]sumcheck.Proof, len(printable[i]))
		for j, printableSumcheck := range printable[i] {
			proof[i][j] = unmarshalSumcheckProof(printableSumcheck)
		}
	}
	return
}

func TestSet(t *testing.T) {

}