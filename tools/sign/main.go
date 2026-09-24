// sign: Ed25519 key generation and manifest signing for the kok-cache auto-update.
//
//	go run ./tools/sign -gen -out <private key file>      prints the PUBLIC key (hex) on stdout
//	go run ./tools/sign -in dist/latest.json               writes dist/latest.json.sig (base64)
//
// The private key (32-byte seed, hex) comes from the file given with -key or from the
// KOK_CACHE_SIGNING_KEY environment variable. It is never printed.
//
// ⛔ The key is NOT a CI secret: it lives only on the maintainer's machine (see
// scripts/sign-release.sh). GitHub Actions builds and attests the release; the maintainer
// signs the manifest that lets installed copies pick it up. Two parties, two keys.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	gen := flag.Bool("gen", false, "generate a key pair")
	out := flag.String("out", "", "with -gen: file receiving the private seed (hex, mode 0600)")
	keyFile := flag.String("key", "", "private seed file (default: $KOK_CACHE_SIGNING_KEY)")
	in := flag.String("in", "", "file to sign; the signature goes to <file>.sig")
	flag.Parse()

	if *gen {
		if *out == "" {
			fail("-gen needs -out")
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err.Error())
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
			fail(err.Error())
		}
		if err := os.WriteFile(*out, []byte(hex.EncodeToString(priv.Seed())), 0o600); err != nil {
			fail(err.Error())
		}
		fmt.Println(hex.EncodeToString(pub))
		return
	}

	if *in == "" {
		fail("-in <file> required")
	}
	var seedHex string
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fail(err.Error())
		}
		seedHex = string(b)
	} else {
		seedHex = os.Getenv("KOK_CACHE_SIGNING_KEY")
	}
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		fail("private key missing or malformed (expected 64 hex chars)")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	data, err := os.ReadFile(*in)
	if err != nil {
		fail(err.Error())
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
	if err := os.WriteFile(*in+".sig", []byte(sig+"\n"), 0o644); err != nil {
		fail(err.Error())
	}
	fmt.Println("signed", *in, "with public key", hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "sign:", msg)
	os.Exit(1)
}
