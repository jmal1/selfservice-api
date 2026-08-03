package unattend

// sha512crypt.go implements the SHA-512 based crypt(3) scheme (the "$6$"
// hashes used by /etc/shadow on modern Linux). We implement it ourselves
// rather than pulling in a dependency: the algorithm is fully specified by
// Ulrich Drepper (https://www.akkadia.org/drepper/SHA-crypt.txt) and the
// implementation is verified against that document's published test vectors
// in sha512crypt_test.go.
//
// A correct hash matters a great deal here: the crypted password is what the
// Ubuntu autoinstall / Debian preseed installers store for the `student`
// account. A wrong hash means students silently cannot log in.

import (
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"strconv"
	"strings"
)

// sha512CryptPrefix is the identifier for SHA-512 crypt hashes.
const sha512CryptPrefix = "$6$"

const (
	sha512RoundsDefault = 5000
	sha512RoundsMin     = 1000
	sha512RoundsMax     = 999999999
	sha512SaltMaxLen    = 16
)

// crypt64Alphabet is the non-standard base64 alphabet used by crypt(3).
const crypt64Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// saltAlphabet is the set of characters permitted in a generated salt.
const saltAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrInvalidSalt is returned when a supplied salt string is not a valid
// SHA-512 crypt salt specification.
var ErrInvalidSalt = errors.New("unattend: invalid sha512 crypt salt")

// SHA512Crypt computes a SHA-512 crypt(3) hash of password using the given
// salt specification. The salt may be a bare salt string or a full
// specification such as "$6$rounds=10000$saltstringsaltstring". The returned
// value is the full "$6$...$..." hash string.
func SHA512Crypt(password, salt string) (string, error) {
	rounds, saltStr, roundsExplicit, err := parseSalt(salt)
	if err != nil {
		return "", err
	}
	return sha512CryptRaw([]byte(password), saltStr, rounds, roundsExplicit), nil
}

// generateSHA512Crypt hashes password with a freshly generated random salt and
// the default round count. This is what the seed generators use.
func generateSHA512Crypt(password string) (string, error) {
	salt, err := randomSalt(sha512SaltMaxLen)
	if err != nil {
		return "", err
	}
	return sha512CryptRaw([]byte(password), salt, sha512RoundsDefault, false), nil
}

// randomSalt returns a cryptographically random salt of length n using the
// crypt salt alphabet.
func randomSalt(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = saltAlphabet[int(b)%len(saltAlphabet)]
	}
	return string(out), nil
}

// parseSalt extracts the round count and salt string from a salt
// specification. It accepts both a bare salt ("saltstring") and a full spec
// ("$6$rounds=10000$saltstring"). roundsExplicit reports whether a rounds=
// parameter was present, which must be echoed back into the output.
func parseSalt(spec string) (rounds int, salt string, roundsExplicit bool, err error) {
	rounds = sha512RoundsDefault
	s := spec
	s = strings.TrimPrefix(s, sha512CryptPrefix)

	if strings.HasPrefix(s, "rounds=") {
		rest := s[len("rounds="):]
		idx := strings.IndexByte(rest, '$')
		var numStr string
		if idx < 0 {
			numStr = rest
			rest = ""
		} else {
			numStr = rest[:idx]
			rest = rest[idx+1:]
		}
		n, convErr := strconv.Atoi(numStr)
		if convErr != nil {
			return 0, "", false, ErrInvalidSalt
		}
		if n < sha512RoundsMin {
			n = sha512RoundsMin
		} else if n > sha512RoundsMax {
			n = sha512RoundsMax
		}
		rounds = n
		roundsExplicit = true
		s = rest
	}

	// The salt runs until the next '$' (if a hash was appended) or the end,
	// and is truncated to at most 16 characters.
	if idx := strings.IndexByte(s, '$'); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > sha512SaltMaxLen {
		s = s[:sha512SaltMaxLen]
	}
	return rounds, s, roundsExplicit, nil
}

// sha512CryptRaw is the core of the algorithm, operating on already-parsed
// inputs. It follows the reference implementation step for step.
func sha512CryptRaw(key []byte, salt string, rounds int, roundsExplicit bool) string {
	saltBytes := []byte(salt)
	keyLen := len(key)
	saltLen := len(saltBytes)

	// Digest A.
	a := sha512.New()
	a.Write(key)
	a.Write(saltBytes)

	// Digest B = SHA512(key + salt + key).
	b := sha512.New()
	b.Write(key)
	b.Write(saltBytes)
	b.Write(key)
	altResult := b.Sum(nil) // 64 bytes

	// Add one byte of B per byte of key.
	for cnt := keyLen; cnt > sha512.Size; cnt -= sha512.Size {
		a.Write(altResult)
	}
	a.Write(altResult[:keyLen%sha512.Size])

	// For each bit of the binary representation of the key length: 1 -> add B,
	// 0 -> add key.
	for cnt := keyLen; cnt > 0; cnt >>= 1 {
		if cnt&1 != 0 {
			a.Write(altResult)
		} else {
			a.Write(key)
		}
	}
	altResult = a.Sum(nil) // intermediate digest A

	// Compute the P byte sequence: DP = SHA512(key repeated keyLen times).
	dp := sha512.New()
	for i := 0; i < keyLen; i++ {
		dp.Write(key)
	}
	tempResult := dp.Sum(nil)
	pBytes := produceSequence(tempResult, keyLen)

	// Compute the S byte sequence: DS = SHA512(salt repeated 16+A[0] times).
	ds := sha512.New()
	rep := 16 + int(altResult[0])
	for i := 0; i < rep; i++ {
		ds.Write(saltBytes)
	}
	tempResult = ds.Sum(nil)
	sBytes := produceSequence(tempResult, saltLen)

	// The main round loop.
	for i := 0; i < rounds; i++ {
		c := sha512.New()
		if i&1 != 0 {
			c.Write(pBytes)
		} else {
			c.Write(altResult)
		}
		if i%3 != 0 {
			c.Write(sBytes)
		}
		if i%7 != 0 {
			c.Write(pBytes)
		}
		if i&1 != 0 {
			c.Write(altResult)
		} else {
			c.Write(pBytes)
		}
		altResult = c.Sum(nil)
	}

	// Encode the result.
	var sb strings.Builder
	sb.WriteString(sha512CryptPrefix)
	if roundsExplicit {
		sb.WriteString("rounds=")
		sb.WriteString(strconv.Itoa(rounds))
		sb.WriteByte('$')
	}
	sb.WriteString(salt)
	sb.WriteByte('$')
	sb.WriteString(encodeSHA512(altResult))
	return sb.String()
}

// produceSequence builds the P or S byte sequences: it repeats the first
// length bytes' worth of temp (a 64-byte digest) out to length bytes.
func produceSequence(temp []byte, length int) []byte {
	out := make([]byte, 0, length)
	for length >= sha512.Size {
		out = append(out, temp...)
		length -= sha512.Size
	}
	out = append(out, temp[:length]...)
	return out
}

// encodeSHA512 performs the crypt(3) base64 encoding of the 64-byte SHA-512
// result using the byte permutation defined for the "$6$" scheme.
func encodeSHA512(r []byte) string {
	var sb strings.Builder
	write := func(b2, b1, b0 byte, n int) {
		w := (uint(b2) << 16) | (uint(b1) << 8) | uint(b0)
		for i := 0; i < n; i++ {
			sb.WriteByte(crypt64Alphabet[w&0x3f])
			w >>= 6
		}
	}

	write(r[0], r[21], r[42], 4)
	write(r[22], r[43], r[1], 4)
	write(r[44], r[2], r[23], 4)
	write(r[3], r[24], r[45], 4)
	write(r[25], r[46], r[4], 4)
	write(r[47], r[5], r[26], 4)
	write(r[6], r[27], r[48], 4)
	write(r[28], r[49], r[7], 4)
	write(r[50], r[8], r[29], 4)
	write(r[9], r[30], r[51], 4)
	write(r[31], r[52], r[10], 4)
	write(r[53], r[11], r[32], 4)
	write(r[12], r[33], r[54], 4)
	write(r[34], r[55], r[13], 4)
	write(r[56], r[14], r[35], 4)
	write(r[15], r[36], r[57], 4)
	write(r[37], r[58], r[16], 4)
	write(r[59], r[17], r[38], 4)
	write(r[18], r[39], r[60], 4)
	write(r[40], r[61], r[19], 4)
	write(r[62], r[20], r[41], 4)
	write(0, 0, r[63], 2)

	return sb.String()
}
