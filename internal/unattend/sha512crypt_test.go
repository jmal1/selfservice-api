package unattend

import "testing"

// TestSHA512Crypt_KnownVectors verifies our SHA-512 crypt implementation
// against the published test vectors from Ulrich Drepper's specification
// (https://www.akkadia.org/drepper/SHA-crypt.txt). If any of these fail the
// implementation is wrong and students would silently be unable to log in.
func TestSHA512Crypt_KnownVectors(t *testing.T) {
	vectors := []struct {
		salt     string
		password string
		expected string
	}{
		{
			salt:     "$6$saltstring",
			password: "Hello world!",
			expected: "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1",
		},
		{
			salt:     "$6$rounds=10000$saltstringsaltstring",
			password: "Hello world!",
			expected: "$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v.",
		},
		{
			salt:     "$6$rounds=5000$toolongsaltstring",
			password: "This is just a test",
			expected: "$6$rounds=5000$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0",
		},
		{
			salt:     "$6$rounds=1400$anotherlongsaltstring",
			password: "a very much longer text to encrypt.  This one even stretches over more" + "than one line.",
			expected: "$6$rounds=1400$anotherlongsalts$POfYwTEok97VWcjxIiSOjiykti.o/pQs.wPvMxQ6Fm7I6IoYN3CmLs66x9t0oSwbtEW7o7UmJEiDwGqd8p4ur1",
		},
		{
			salt:     "$6$rounds=77777$short",
			password: "we have a short salt string but not a short password",
			expected: "$6$rounds=77777$short$WuQyW2YR.hBNpjjRhpYD/ifIw05xdfeEyQoMxIXbkvr0gge1a1x3yRULJ5CCaUeOxFmtlcGZelFl5CxtgfiAc0",
		},
		{
			salt:     "$6$rounds=123456$asaltof16chars..",
			password: "a short string",
			expected: "$6$rounds=123456$asaltof16chars..$BtCwjqMJGx5hrJhZywWvt0RLE8uZ4oPwcelCjmw2kSYu.Ec6ycULevoBK25fs2xXgMNrCzIMVcgEJAstJeonj1",
		},
		{
			// rounds below the minimum are clamped to 1000 and the
			// output echoes the clamped value.
			salt:     "$6$rounds=10$roundstoolow",
			password: "the minimum number is still observed",
			expected: "$6$rounds=1000$roundstoolow$kUMsbe306n21p9R.FRkW3IGn.S9NPN0x50YhH1xhLsPuWGsUSklZt58jaTfF4ZEQpyUNGc0dqbpBYYBaHHrsX.",
		},
	}

	for _, v := range vectors {
		got, err := SHA512Crypt(v.password, v.salt)
		if err != nil {
			t.Fatalf("SHA512Crypt(%q, %q) returned error: %v", v.password, v.salt, err)
		}
		if got != v.expected {
			t.Errorf("SHA512Crypt(%q, %q)\n  got:  %s\n  want: %s", v.password, v.salt, got, v.expected)
		}
	}
}

// TestGenerateSHA512Crypt_Format verifies generated hashes have the $6$
// prefix, a non-empty salt, and never contain the plaintext password.
func TestGenerateSHA512Crypt_Format(t *testing.T) {
	const pw = "SuperSecretPlaintext-DoNotLeak-42!"
	h, err := generateSHA512Crypt(pw)
	if err != nil {
		t.Fatalf("generateSHA512Crypt returned error: %v", err)
	}
	if len(h) < len("$6$x$") || h[:3] != "$6$" {
		t.Fatalf("hash does not start with $6$: %q", h)
	}
	if contains(h, pw) {
		t.Fatalf("hash leaks plaintext password: %q", h)
	}
	// Two generations should differ (random salt).
	h2, _ := generateSHA512Crypt(pw)
	if h == h2 {
		t.Errorf("two generated hashes were identical, salt is not random")
	}
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
