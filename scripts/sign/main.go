// Command sign is a release helper for vlgr's signed self-update. It shares
// crypto/ed25519 with the verifier in internal/selfupdate, so no external
// tooling is needed.
//
// Usage:
//
//	go run ./scripts/sign genkey            # print a fresh private seed + public key
//	go run ./scripts/sign pubkey  <seedhex> # derive the public key (hex) from a seed
//	go run ./scripts/sign sign <seedhex> <file>...  # write <file>.sig for each file
//
// The 32-byte seed hex is the private key: keep it secret (e.g. a CI secret in
// $VLGR_SIGNING_KEY). The public key hex is compiled into release binaries via
// -X vlgr/internal/selfupdate.SigningPublicKey; each <file>.sig holds the hex
// ed25519 signature the updater checks before installing.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: sign genkey|pubkey|sign ...")
	}
	switch os.Args[1] {
	case "genkey":
		genkey()
	case "pubkey":
		if len(os.Args) != 3 {
			fail("usage: sign pubkey <seedhex>")
		}
		fmt.Println(hex.EncodeToString(privFromSeed(os.Args[2]).Public().(ed25519.PublicKey)))
	case "sign":
		if len(os.Args) < 4 {
			fail("usage: sign sign <seedhex> <file>...")
		}
		signFiles(privFromSeed(os.Args[2]), os.Args[3:])
	default:
		fail("unknown command %q", os.Args[1])
	}
}

func genkey() {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		fail("generate seed: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	fmt.Printf("private (seed, keep secret): %s\n", hex.EncodeToString(seed))
	fmt.Printf("public  (compile in):        %s\n", hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
}

func privFromSeed(seedHex string) ed25519.PrivateKey {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil {
		fail("decode seed: %v", err)
	}
	if len(seed) != ed25519.SeedSize {
		fail("seed length %d, want %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func signFiles(priv ed25519.PrivateKey, files []string) {
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fail("read %s: %v", f, err)
		}
		sig := ed25519.Sign(priv, data)
		if err := os.WriteFile(f+".sig", []byte(hex.EncodeToString(sig)), 0644); err != nil {
			fail("write %s.sig: %v", f, err)
		}
		fmt.Printf("signed %s -> %s.sig\n", f, f)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
