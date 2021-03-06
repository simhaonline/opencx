package match

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"math/big"
	"sync"

	gmpbig "github.com/Rjected/gmp"
	"github.com/Rjected/lit/crypto/koblitz"
	"github.com/mit-dci/opencx/crypto/timelockencoders"
	"github.com/mit-dci/opencx/logging"
	"golang.org/x/crypto/sha3"
)

// TODO: CLEAN UP THIS CODE!!!

// Transcript is the representation of a non front running proof
// transcript. Puzzled orders are the "batch" and this should be able
// to be verified quickly.
type Transcript struct {
	BatchId       AuctionID           `json:batchid`
	BatchIdSig    []byte              `json:"signature"`
	PuzzledOrders []SignedEncSolOrder `json:"puzzledorders"`
	Commitment    [32]byte            `json:"commitment"`
	CommitSig     []byte              `json:"commitsig"`
	Responses     []CommitResponse    `json:"responses"`
	Solutions     []AuctionOrder      `json:"solutions"`
}

// Verify verifies the signatures in the transcript and ensures
// that the batch was carried out correctly. In this implementation,
// the exchange is signing the set of all orders in plaintext, so the
// 'e' value in the signature is the hash of all of the orders.
func (tr *Transcript) Verify() (valid bool, err error) {
	// First verify batch ID
	hasher := sha3.New256()
	if _, err = hasher.Write(tr.BatchId[:]); err != nil {
		err = fmt.Errorf("Error writing batch id to hasher: %s", err)
		return
	}

	// e is the hash of the batch ID
	e := hasher.Sum(nil)
	hasher.Reset()

	var exchangePubKey *koblitz.PublicKey
	if exchangePubKey, _, err = koblitz.RecoverCompact(koblitz.S256(), tr.BatchIdSig, e); err != nil {
		err = fmt.Errorf("Error recovering pubkey from batch sig: %s", err)
		return
	}

	// map of PKH to PK
	var pubkeyMap map[[32]byte]SignedEncSolOrder = make(map[[32]byte]SignedEncSolOrder)
	var tempPKH [32]byte = [32]byte{}
	var zeroBuf [32]byte = [32]byte{}
	var bufForCommitment []byte
	for _, pzOrder := range tr.PuzzledOrders {
		hasher.Reset()
		copy(tempPKH[:], zeroBuf[:])
		var pzBuf []byte
		if pzBuf, err = pzOrder.Serialize(); err != nil {
			err = fmt.Errorf("Error serializing puzzle order for transcript verification: %s", err)
			return
		}

		var notSignedPzBuf []byte
		if notSignedPzBuf, err = pzOrder.EncSolOrder.Serialize(); err != nil {
			err = fmt.Errorf("Error serializing unsigned part of puzzle order: %s", err)
			return
		}

		if _, err = hasher.Write(notSignedPzBuf); err != nil {
			err = fmt.Errorf("Error writing puzzle order to hasher: %s", err)
			return
		}
		hashOrder := hasher.Sum(nil)

		var firstUserPubKey *koblitz.PublicKey
		if firstUserPubKey, _, err = koblitz.RecoverCompact(koblitz.S256(), pzOrder.Signature, hashOrder); err != nil {
			err = fmt.Errorf("Error recovering user pubkey from sig: %s", err)
			return
		}
		hasher.Reset()
		hasher.Write(firstUserPubKey.SerializeCompressed())
		copy(tempPKH[:], hasher.Sum(nil))

		// only add to map if the signature checks out -- otherwise,
		// it could have been modified by some adversary on the way to
		// the exchange. This is OK because the exchange is making a
		// commitment hopefully before puzzles can be solved - and
		// including signatures in the commitment, which is why we
		// serialize the entire signed order.
		pubkeyMap[tempPKH] = pzOrder
		bufForCommitment = append(bufForCommitment, pzBuf...)
	}

	hasher.Reset()
	if _, err = hasher.Write(bufForCommitment); err != nil {
		err = fmt.Errorf("Error writing puzzles for commitment to hasher: %s", err)
		return
	}

	// hash of the puzzled orders
	e2 := hasher.Sum(nil)
	hasher.Reset()

	var otherExchangePubkey *koblitz.PublicKey
	if otherExchangePubkey, _, err = koblitz.RecoverCompact(koblitz.S256(), tr.CommitSig, tr.Commitment[:]); err != nil {
		err = fmt.Errorf("Error recovering pubkey for commit signature: %s", err)
		return
	}

	if !otherExchangePubkey.IsEqual(exchangePubKey) {
		err = fmt.Errorf("Exchange used different pubkey for signing commitment versus batchid")
		return
	}
	// var exsig *koblitz.Signature
	// if exsig, err = koblitz.ParseSignature(tr.CommitSig, koblitz.S256()); err != nil {
	// 	err = fmt.Errorf("Error parsing commitment signature: %s", err)
	// 	return
	// }

	// if !exsig.Verify(e2, exchangePubKey) {
	// 	err = fmt.Errorf("Invalid commitment signature from exchange")
	// 	return
	// }

	if bytes.Compare(e2, tr.Commitment[:]) != 0 {
		err = fmt.Errorf("Commitment is not equal to hash of orders - invalid transcript")
		return
	}

	var e3Buf [][32]byte = make([][32]byte, len(tr.Responses))
	var errChan chan error = make(chan error, len(tr.Responses))
	var hashCommWg sync.WaitGroup
	hashCommWg.Add(len(tr.Responses))

	for i, response := range tr.Responses {
		go func(j int, comm [32]byte, commSig []byte, res CommitResponse) {
			var cErr error
			e3Buf[j] = [32]byte{}
			currHasher := sha3.New256()
			if _, cErr = currHasher.Write(comm[:]); cErr != nil {
				errChan <- fmt.Errorf("Error writing commitment to hasher: %s", cErr)
				hashCommWg.Done()
				return
			}
			if _, cErr = currHasher.Write(commSig); cErr != nil {
				errChan <- fmt.Errorf("Error writing commitment sig to hasher: %s", cErr)
				hashCommWg.Done()
				return
			}
			var answerBytes []byte
			if answerBytes, cErr = res.PuzzleAnswerReveal.Serialize(); cErr != nil {
				errChan <- fmt.Errorf("Error serializing answer: %s", cErr)
				hashCommWg.Done()
				return
			}
			if _, cErr = currHasher.Write(answerBytes); cErr != nil {
				errChan <- fmt.Errorf("Error writing answerBytes to hasher: %s", cErr)
				hashCommWg.Done()
				return
			}
			copy(e3Buf[j][:], currHasher.Sum(nil))
			hashCommWg.Done()
		}(i, tr.Commitment, tr.CommitSig, response)
	}
	hashCommWg.Wait()

	select {
	case nonNilErr := <-errChan:
		err = fmt.Errorf("Error with goroutine for hashing: %s", nonNilErr)
		return
	default:
	}

	var ok bool
	for _, response := range tr.Responses {
		ok = false
		copy(tempPKH[:], zeroBuf[:])
		hasher.Reset()
		// h(comm + sig + answer) = e
		if _, err = hasher.Write(tr.Commitment[:]); err != nil {
			err = fmt.Errorf("Error writing commitment to hasher: %s", err)
			return
		}
		if _, err = hasher.Write(tr.CommitSig); err != nil {
			err = fmt.Errorf("Error writing commit sig to hasher: %s", err)
			return
		}

		var answerBytes []byte
		if answerBytes, err = response.PuzzleAnswerReveal.Serialize(); err != nil {
			err = fmt.Errorf("Error serializing answer: %s", err)
			return
		}
		if _, err = hasher.Write(answerBytes); err != nil {
			err = fmt.Errorf("Error writing answer bytes to hasher: %s", err)
			return
		}

		e3 := hasher.Sum(nil)

		var userPubKey *koblitz.PublicKey
		if userPubKey, _, err = koblitz.RecoverCompact(koblitz.S256(), response.CommResponseSig[:], e3); err != nil {
			err = fmt.Errorf("Error recovering user pubkey from signature: %s", err)
			return
		}
		hasher.Reset()
		hasher.Write(userPubKey.SerializeCompressed())
		copy(tempPKH[:], hasher.Sum(nil))

		// now we get the order and check that it was included. Also
		// check that p * q = N in puzzle, but only log it
		var currEnc SignedEncSolOrder
		if currEnc, ok = pubkeyMap[tempPKH]; !ok {
			err = fmt.Errorf("Error, user pubkey not in previous map, so it's a signature without an order")
			return
		}

		tempN := new(big.Int).Mul(response.PuzzleAnswerReveal.Q, response.PuzzleAnswerReveal.P)
		if tempN.Cmp(currEnc.EncSolOrder.OrderPuzzle.N) != 0 {
			logging.Warnf("User included incorrect factors in order, this order will require some solving")
		}

	}

	valid = true
	return
}

// Solve processes the encrypted solution orders and the commitment
// Responses to partition the encrypted orders into those solvable by
// Responses and those that are unsolvable.
func (tr *Transcript) Solve() (solvedOrders []AuctionOrder, invalidResponses []CommitResponse, err error) {
	// TODO: optimize for garbage collection by using a single [32]byte
	// pool for hashing
	// this is a map from hash(N) to the order
	hasher := sha3.New256()
	var pzMap map[[32]byte]EncryptedSolutionOrder = make(map[[32]byte]EncryptedSolutionOrder)
	var tempNSum [32]byte = [32]byte{}
	var zeroBuf [32]byte = [32]byte{}
	for _, pzOrder := range tr.PuzzledOrders {
		hasher.Reset()
		copy(tempNSum[:], zeroBuf[:])

		// hash of N's bytes
		hasher.Write(pzOrder.EncSolOrder.OrderPuzzle.N.Bytes())
		copy(tempNSum[:], hasher.Sum(nil))

		pzMap[tempNSum] = pzOrder.EncSolOrder
	}

	NBuf := make([][]byte, len(tr.Responses))
	// precompute N values for i
	var groupComputeN sync.WaitGroup
	groupComputeN.Add(len(tr.Responses))
	for i, ans := range tr.Responses {
		go func(j int, answer CommitResponse) {
			pgmp := new(gmpbig.Int).SetBytes(answer.PuzzleAnswerReveal.P.Bytes())
			qgmp := new(gmpbig.Int).SetBytes(answer.PuzzleAnswerReveal.Q.Bytes())
			NBuf[j] = new(gmpbig.Int).Mul(pgmp, qgmp).Bytes()
			groupComputeN.Done()
		}(i, ans)
	}
	groupComputeN.Wait()

	var solutionMap map[CommitResponse]EncryptedSolutionOrder = make(map[CommitResponse]EncryptedSolutionOrder)
	var currEnc EncryptedSolutionOrder
	var ok bool
	for j, answer := range tr.Responses {
		ok = false
		hasher.Reset()
		copy(tempNSum[:], zeroBuf[:])

		// hash of N's bytes
		hasher.Write(NBuf[j])
		copy(tempNSum[:], hasher.Sum(nil))

		if currEnc, ok = pzMap[tempNSum]; ok {
			solutionMap[answer] = currEnc
		} else {
			invalidResponses = append(invalidResponses, answer)
		}
	}

	solvedOrders = make([]AuctionOrder, len(solutionMap))
	errChan := make(chan error, len(solutionMap))

	// TODO: parallelize this
	var solveWg sync.WaitGroup
	solveWg.Add(len(solutionMap))
	i := 0
	for response, encOrder := range solutionMap {
		go func(j int, p, q *big.Int, currEncOrder EncryptedSolutionOrder) {
			var currAuctionOrder AuctionOrder
			var currErr error
			if currAuctionOrder, currErr = trapdoor(p, q, currEncOrder); currErr != nil {
				errChan <- fmt.Errorf("Error running %dth trapdoor for revealed answer: %s", j, currErr)
			}
			solvedOrders[j] = currAuctionOrder
			solveWg.Done()
		}(i, response.PuzzleAnswerReveal.P, response.PuzzleAnswerReveal.Q, encOrder)
		i++
	}
	solveWg.Wait()

	// if there is an error, catch it and return
	select {
	case nonNilErr := <-errChan:
		err = fmt.Errorf("Error with parallel trapdoor: %s", nonNilErr)
		return
	default:
	}

	return
}

// calculate trapdoor to solve puzzle
func trapdoor(p, q *big.Int, encOrder EncryptedSolutionOrder) (order AuctionOrder, err error) {
	// calculate trapdoor e = 2^t mod phi(n) = 2^t mod (p-1)(q-1)
	two := gmpbig.NewInt(2)
	one := gmpbig.NewInt(1)
	pgmp := new(gmpbig.Int).SetBytes(p.Bytes())
	qgmp := new(gmpbig.Int).SetBytes(q.Bytes())
	agmp := new(gmpbig.Int).SetBytes(encOrder.OrderPuzzle.A.Bytes())
	tgmp := new(gmpbig.Int).SetBytes(encOrder.OrderPuzzle.T.Bytes())
	ngmp := new(gmpbig.Int).SetBytes(encOrder.OrderPuzzle.N.Bytes())
	ckgmp := new(gmpbig.Int).SetBytes(encOrder.OrderPuzzle.CK.Bytes())
	pminusone := new(gmpbig.Int).Sub(pgmp, one)
	qminusone := new(gmpbig.Int).Sub(qgmp, one)
	phi := new(gmpbig.Int).Mul(pminusone, qminusone)
	e := new(gmpbig.Int).Exp(two, tgmp, phi)

	// calculate b = a^e mod N
	b := new(gmpbig.Int).Exp(agmp, e, ngmp)

	// now b xor c_k = k
	k := new(gmpbig.Int).Xor(b, ckgmp)
	kBytes := k.Bytes()

	var key []byte
	if len(kBytes) <= 16 {
		key = make([]byte, 16)
	} else {
		key = make([]byte, len(kBytes))
	}
	copy(key, kBytes)

	var orderBytes []byte
	if orderBytes, err = timelockencoders.DecryptPuzzleRC5(encOrder.OrderCiphertext, key); err != nil {
		err = fmt.Errorf("Error decrypting rc5 puzzle from trapdoor key: %s", err)
		return
	}

	if err = order.Deserialize(orderBytes); err != nil {
		err = fmt.Errorf("Error deserializing order for trapdoor into struct: %s", err)
		return
	}
	return
}

// Serialize uses gob encoding to turn the transcript into bytes.
func (tr *Transcript) Serialize() (raw []byte, err error) {
	var b bytes.Buffer

	// register Transcript interface
	gob.Register(Transcript{})

	// create a new encoder writing to the buffer
	enc := gob.NewEncoder(&b)

	// encode the transcript in the buffer
	if err = enc.Encode(tr); err != nil {
		err = fmt.Errorf("Error encoding transcript: %s", err)
		return
	}

	// Get the bytes from the buffer
	raw = b.Bytes()
	return
}

// Deserialize turns the transcript from bytes into a usable
// struct.
func (tr *Transcript) Deserialize(raw []byte) (err error) {
	var b *bytes.Buffer
	b = bytes.NewBuffer(raw)

	// register Transcript
	gob.Register(Transcript{})

	// create a new decoder writing to the buffer
	dec := gob.NewDecoder(b)

	// decode the transcript in the buffer
	if err = dec.Decode(tr); err != nil {
		err = fmt.Errorf("Error decoding transcript: %s", err)
		return
	}

	return
}
