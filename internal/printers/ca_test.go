package printers

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Fingerprints of the bundled CA files, as listed in ca/SOURCES.md.
var wantCA = map[string]string{
	"BBL CA":                "sha256:030bca81cece18b7eff3cfd2b75d09d3efca893bc069609e37fa04257fe4d840",
	"BBL CA2 RSA":           "sha256:e98f19578b3f124ace6b8a247ffeda52dc99c89fd4e7d20c82829977b7f33502",
	"BBL CA2 ECC":           "sha256:1f99d846718c8d333ac319cb8e70919ab63236090fbadde909ba4a6f0867a683",
	"BBL CA2 RSA cross":     "sha256:3d13019b45a4fa8a6cb8dc0fd7becbb19bb2bf0c2fcb9434a9345fa58680da0b",
	"BBL CA2 ECC cross":     "sha256:e898483224f9092f575d627433bb21ed9d125a77e3635b0da6fc18fe850a02d7",
	"BBL Device CA O1C2-V2": "sha256:ce667b6c001d26c34ed97333fb6099699409fc3d4e12429c37baebb05c46c60a",
	"BBL Device CA N7-V2":   "sha256:6a854d546ed6e3ce7a2112657e53f4555374413e6f5cb55f995d1ddd7c06dc69",
	"BBL Device CA N6-V2":   "sha256:68bf3182bc32d5a5454f864928abaa291941f6d5ac6a86a0e5ad6ecfe2d5477b",
}

func TestBundledCAs(t *testing.T) {
	b, err := LoadBundle()
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	roots := 0
	for _, c := range b.Certs {
		got = append(got, Fingerprint(c))
		if string(c.RawSubject) == string(c.RawIssuer) {
			roots++
		}
		if !c.IsCA {
			t.Errorf("%s is not a CA", c.Subject.CommonName)
		}
	}
	for _, fp := range wantCA {
		want = append(want, fp)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("bundle fingerprints\n got %v\nwant %v", got, want)
	}
	if roots != 3 {
		t.Fatalf("want 3 roots, got %d", roots)
	}
	doc, err := os.ReadFile("ca/SOURCES.md")
	if err != nil {
		t.Fatal(err)
	}
	listed := regexp.MustCompile(`\| ([0-9a-f]{64}) \|`).FindAllStringSubmatch(string(doc), -1)
	if len(listed) != len(wantCA) {
		t.Fatalf("SOURCES.md lists %d fingerprints", len(listed))
	}
	for _, m := range listed {
		found := false
		for _, fp := range got {
			if fp == "sha256:"+m[1] {
				found = true
			}
		}
		if !found {
			t.Errorf("SOURCES.md fingerprint %s not in bundle", m[1])
		}
	}
}

func TestBundleRejectsNonCA(t *testing.T) {
	if _, err := BundleFromPEM([]byte("-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n")); err == nil {
		t.Fatal("non certificate PEM accepted")
	}
	if _, err := BundleFromPEM(nil); err == nil {
		t.Fatal("empty bundle accepted")
	}
}
