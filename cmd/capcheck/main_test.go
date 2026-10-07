package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func capcheck(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String() + errb.String()
}

func writeCatalog(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func goodCatalog(t *testing.T) string {
	b, err := os.ReadFile("testdata/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCleanCatalogPasses(t *testing.T) {
	code, out := capcheck(t, "--catalog", "testdata/catalog.yaml", "--proto", "testdata/proto", "--package", "billing.v1", "--module", "billing")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "3 operations, 3 rpcs") {
		t.Fatalf("output = %s", out)
	}
}

// Without --package, the synced copy of another contract is the Core's too,
// and its RPC is reported.
func TestWithoutPackageEveryServiceCounts(t *testing.T) {
	code, out := capcheck(t, "--catalog", "testdata/catalog.yaml", "--proto", "testdata/proto")
	if code != 1 || !strings.Contains(out, "rpc vendor.v1.VendorService/Ping is not catalogued") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestUncataloguedRPCFails(t *testing.T) {
	body := goodCatalog(t)
	// Drop the VoidInvoice entry.
	i := strings.Index(body, "- id: billing.invoice.void")
	j := strings.Index(body, "- id: billing.invoice.watch")
	p := writeCatalog(t, body[:i]+body[j:])
	code, out := capcheck(t, "--catalog", p, "--proto", "testdata/proto", "--package", "billing.v1")
	if code != 1 || !strings.Contains(out, "rpc billing.v1.InvoiceService/VoidInvoice is not catalogued") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCataloguedRPCThatDoesNotExistFails(t *testing.T) {
	p := writeCatalog(t, goodCatalog(t)+`- id: billing.invoice.purge
  rpc: billing.v1.InvoiceService/PurgeInvoices
  action: billing.invoice.purge
  effect: delete
  risk: critical
  request: billing.v1.ListInvoicesRequest
  response: google.protobuf.Empty
`)
	code, out := capcheck(t, "--catalog", p, "--proto", "testdata/proto", "--package", "billing.v1")
	if code != 1 || !strings.Contains(out, `rpc "billing.v1.InvoiceService/PurgeInvoices" is not declared`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestWrongMessageFails(t *testing.T) {
	p := writeCatalog(t, strings.Replace(goodCatalog(t), "request: billing.v1.Outer.Inner", "request: billing.v1.Inner", 1))
	code, out := capcheck(t, "--catalog", p, "--proto", "testdata/proto", "--package", "billing.v1")
	if code != 1 || !strings.Contains(out, `takes "billing.v1.Outer.Inner"`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestDuplicateFails(t *testing.T) {
	// The same id twice is an error; the same action on two RPCs is not.
	p := writeCatalog(t, strings.Replace(goodCatalog(t), "id: billing.invoice.void", "id: billing.invoice.list", 1))
	code, out := capcheck(t, "--catalog", p, "--proto", "testdata/proto", "--package", "billing.v1")
	if code != 1 || !strings.Contains(out, "already listed by entry 1") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestUngatedIsAWarning(t *testing.T) {
	p := writeCatalog(t, strings.Replace(goodCatalog(t), "action: billing.invoice.void", `action: ""`, 1))
	code, out := capcheck(t, "--catalog", p, "--proto", "testdata/proto", "--package", "billing.v1")
	if code != 0 || !strings.Contains(out, "does not call the gate") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestModuleMismatchFails(t *testing.T) {
	code, out := capcheck(t, "--catalog", "testdata/catalog.yaml", "--proto", "testdata/proto", "--package", "billing.v1", "--module", "invoices", "--module", "payments")
	if code != 1 || !strings.Contains(out, `action module "billing" is not one of [invoices payments]`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestMalformedCatalogFails(t *testing.T) {
	p := writeCatalog(t, strings.Replace(goodCatalog(t), "risk: high", "risk: severe", 1))
	code, out := capcheck(t, "--catalog", p, "--proto", "testdata/proto", "--package", "billing.v1")
	if code != 1 || !strings.Contains(out, `risk "severe"`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	if code, out := capcheck(t, "--catalog", "testdata/catalog.yaml"); code != 2 {
		t.Fatalf("no --proto: exit %d: %s", code, out)
	}
	if code, out := capcheck(t, "--catalog", "testdata/nope.yaml", "--proto", "testdata/proto"); code != 2 {
		t.Fatalf("missing catalog: exit %d: %s", code, out)
	}
	if code, out := capcheck(t, "--catalog", "testdata/catalog.yaml", "--proto", "testdata/nope"); code != 2 {
		t.Fatalf("missing proto dir: exit %d: %s", code, out)
	}
	bad := filepath.Join(t.TempDir(), "bad.proto")
	_ = os.WriteFile(bad, []byte("syntax = \"proto3\"; service {"), 0o600)
	if code, out := capcheck(t, "--catalog", "testdata/catalog.yaml", "--proto", filepath.Dir(bad)); code != 2 {
		t.Fatalf("unparsable proto: exit %d: %s", code, out)
	}
}

func TestResolve(t *testing.T) {
	known := map[string]bool{"billing.v1.A": true, "billing.v1.Outer.Inner": true, "billing.Shared": true}
	cases := map[string]string{
		"A":                     "billing.v1.A",
		".billing.v1.A":         "billing.v1.A",
		"Outer.Inner":           "billing.v1.Outer.Inner",
		"Shared":                "billing.Shared",
		"v1.A":                  "billing.v1.A",
		"google.protobuf.Empty": "google.protobuf.Empty",
		"Unknown":               "billing.v1.Unknown",
	}
	for ref, want := range cases {
		if got := resolve(known, "billing.v1", ref); got != want {
			t.Errorf("resolve(%q) = %q, want %q", ref, got, want)
		}
	}
}
