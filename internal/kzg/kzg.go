package kzg

import (
	"errors"
	"math/big"

	"github.com/consensys/gnark-crypto/ecc"
	// TODO: use bls12381 alias instead of curve everywhere
	// bls12381 "github.com/consensys/gnark-crypto/ecc/bls12-381"
	curve "github.com/consensys/gnark-crypto/ecc/bls12-381"
	"github.com/consensys/gnark-crypto/ecc/bls12-381/fr"
)

type Commitment = curve.G1Affine
type Polynomial = []fr.Element

var (
	ErrInvalidNbDigests              = errors.New("number of digests is not the same as the number of polynomials")
	ErrInvalidPolynomialSize         = errors.New("invalid polynomial size (larger than SRS or == 0)")
	ErrVerifyOpeningProof            = errors.New("can't verify opening proof")
	ErrVerifyBatchOpeningSinglePoint = errors.New("can't verify batch opening proof at single point")
)

// Proof to the claim that a polynomial f(x) was evaluated at a point `a` and
// resulted in `f(a)`
type OpeningProof struct {
	// H quotient polynomial (f - f(a))/(x-a)
	QuotientComm curve.G1Affine

	// Point that we are evaluating the polynomial at : `a`
	InputPoint fr.Element

	// ClaimedValue purported value : `f(a)`
	ClaimedValue fr.Element
}

// Verify a KZG proof
//
// Copied from gnark-crypto with minor modifications
func Verify(commitment *Commitment, proof *OpeningProof, open_key *OpeningKey) error {

	// [f(a)]G₁
	var claimedValueG1Aff curve.G1Jac
	var claimedValueBigInt big.Int
	proof.ClaimedValue.BigInt(&claimedValueBigInt)
	claimedValueG1Aff.ScalarMultiplicationAffine(&open_key.GenG1, &claimedValueBigInt)

	// [f(α) - f(a)]G₁
	var fminusfaG1Jac curve.G1Jac
	fminusfaG1Jac.FromAffine(commitment)
	fminusfaG1Jac.SubAssign(&claimedValueG1Aff)

	// [-H(α)]G₁
	var negH curve.G1Affine
	negH.Neg(&proof.QuotientComm)

	// [α-a]G₂
	var alphaMinusaG2Jac, genG2Jac, alphaG2Jac curve.G2Jac
	var pointBigInt big.Int
	proof.InputPoint.BigInt(&pointBigInt)
	genG2Jac.FromAffine(&open_key.GenG2)
	alphaG2Jac.FromAffine(&open_key.AlphaG2)
	alphaMinusaG2Jac.ScalarMultiplication(&genG2Jac, &pointBigInt).
		Neg(&alphaMinusaG2Jac).
		AddAssign(&alphaG2Jac)

	// [α-a]G₂
	var xminusaG2Aff curve.G2Affine
	xminusaG2Aff.FromJacobian(&alphaMinusaG2Jac)

	// [f(α) - f(a)]G₁
	var fminusfaG1Aff curve.G1Affine
	fminusfaG1Aff.FromJacobian(&fminusfaG1Jac)

	// e([f(α) - f(a)]G₁, G₂).e([-H(α)]G₁, [α-a]G₂) ==? 1
	check, err := curve.PairingCheck(
		[]curve.G1Affine{fminusfaG1Aff, negH},
		[]curve.G2Affine{open_key.GenG2, xminusaG2Aff},
	)
	if err != nil {
		return err
	}
	if !check {
		return ErrVerifyOpeningProof
	}
	return nil
}

// Create a KZG proof that a polynomial f(x) when evaluated at a point `a` is equal to `f(a)`
func Open(domain *Domain, p Polynomial, point fr.Element, ck *CommitKey) (OpeningProof, error) {
	if len(p) == 0 || len(p) > len(ck.G1) {
		return OpeningProof{}, ErrInvalidPolynomialSize
	}
	output_point, err := EvaluateLagrangePolynomial(domain, p, point)
	if err != nil {
		return OpeningProof{}, err
	}

	res := OpeningProof{
		InputPoint:   point,
		ClaimedValue: *output_point,
	}

	// compute the quotient polynomial
	quotient_poly, err := DividePolyByXminusA(*domain, p, res.ClaimedValue, point)
	if err != nil {
		return OpeningProof{}, err
	}

	// commit to Quotient polynomial
	quotientCommit, err := Commit(quotient_poly, ck)
	if err != nil {
		return OpeningProof{}, err
	}
	res.QuotientComm.Set(quotientCommit)

	return res, nil
}

// DividePolyByXminusA computes (f-f(a))/(x-a), in canonical basis, in regular form
// Note: polynomial is in lagrange basis
func DividePolyByXminusA(domain Domain, f Polynomial, fa, a fr.Element) ([]fr.Element, error) {

	if domain.Cardinality != uint64(len(f)) {
		return nil, errors.New("polynomial size does not match domain size")
	}

	if domain.isInDomain(a) {
		return nil, errors.New("cannot divide by point in the domain")
	}

	// first we compute f-f(a)
	numer := make([]fr.Element, len(f))
	for i := 0; i < len(f); i++ {
		numer[i].Sub(&f[i], &fa)
	}

	// Now compute 1/(roots - a)
	denom := make([]fr.Element, len(f))
	for i := 0; i < len(f); i++ {
		denom[i].Sub(&domain.Roots[i], &a)
	}
	denom = fr.BatchInvert(denom)

	for i := 0; i < len(f); i++ {
		denom[i].Mul(&denom[i], &numer[i])
	}

	return denom, nil
}

// Copied from gnark-crypto
// TODO: need to modify naming of digests and comments
func BatchVerifyMultiPoints(commitments []Commitment, proofs []OpeningProof, open_key *OpeningKey) error {

	// check consistancy nb proogs vs nb digests
	if len(commitments) != len(proofs) {
		return ErrInvalidNbDigests
	}

	// if only one digest, call Verify
	if len(commitments) == 1 {
		return Verify(&commitments[0], &proofs[0], open_key)
	}

	// sample random numbers for sampling
	randomNumbers := make([]fr.Element, len(commitments))
	randomNumbers[0].SetOne()
	for i := 1; i < len(randomNumbers); i++ {
		_, err := randomNumbers[i].SetRandom()
		if err != nil {
			return err
		}
	}

	// combine random_i*quotient_i
	var foldedQuotients curve.G1Affine
	quotients := make([]curve.G1Affine, len(proofs))
	for i := 0; i < len(randomNumbers); i++ {
		quotients[i].Set(&proofs[i].QuotientComm)
	}
	config := ecc.MultiExpConfig{}
	_, err := foldedQuotients.MultiExp(quotients, randomNumbers, config)
	if err != nil {
		return nil
	}

	// fold digests and evals
	evals := make([]fr.Element, len(commitments))
	for i := 0; i < len(randomNumbers); i++ {
		evals[i].Set(&proofs[i].ClaimedValue)
	}
	foldedDigests, foldedEvals, err := fold(commitments, evals, randomNumbers)
	if err != nil {
		return err
	}

	// compute commitment to folded Eval
	var foldedEvalsCommit curve.G1Affine
	var foldedEvalsBigInt big.Int
	foldedEvals.BigInt(&foldedEvalsBigInt)
	foldedEvalsCommit.ScalarMultiplication(&open_key.GenG1, &foldedEvalsBigInt)

	// compute F = foldedDigests - foldedEvalsCommit
	foldedDigests.Sub(&foldedDigests, &foldedEvalsCommit)

	// combine random_i*(point_i*quotient_i)
	var foldedPointsQuotients curve.G1Affine
	for i := 0; i < len(randomNumbers); i++ {
		randomNumbers[i].Mul(&randomNumbers[i], &proofs[i].InputPoint)
	}
	_, err = foldedPointsQuotients.MultiExp(quotients, randomNumbers, config)
	if err != nil {
		return err
	}

	// lhs first pairing
	foldedDigests.Add(&foldedDigests, &foldedPointsQuotients)

	// lhs second pairing
	foldedQuotients.Neg(&foldedQuotients)

	// pairing check
	check, err := curve.PairingCheck(
		[]curve.G1Affine{foldedDigests, foldedQuotients},
		[]curve.G2Affine{open_key.GenG2, open_key.AlphaG2},
	)
	if err != nil {
		return err
	}
	if !check {
		return ErrVerifyOpeningProof
	}
	return nil

}

// Copied from gnark-crypto
// TODO: need to modify naming of digests and comments
func fold(digests []Commitment, evaluations []fr.Element, factors []fr.Element) (Commitment, fr.Element, error) {

	// length inconsistancy between digests and evaluations should have been done before calling this function
	nbDigests := len(digests)

	// fold the claimed values
	var foldedEvaluations, tmp fr.Element
	for i := 0; i < nbDigests; i++ {
		tmp.Mul(&evaluations[i], &factors[i])
		foldedEvaluations.Add(&foldedEvaluations, &tmp)
	}

	// fold the digests
	var foldedDigests Commitment
	_, err := foldedDigests.MultiExp(digests, factors, ecc.MultiExpConfig{})
	if err != nil {
		return foldedDigests, foldedEvaluations, err
	}

	// folding done
	return foldedDigests, foldedEvaluations, nil

}
