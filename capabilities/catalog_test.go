package capabilities

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
- id: billing.invoice.list
  rpc: billing.v1.InvoiceService/ListInvoices
  action: billing.invoice.list
  effect: read
  risk: low
  paginated: true
  request: billing.v1.ListInvoicesRequest
  response: billing.v1.ListInvoicesResponse
- id: billing.invoice.void
  rpc: billing.v1.InvoiceService/VoidInvoice
  action: billing.invoice.void
  effect: write
  risk: high
  request: billing.v1.VoidInvoiceRequest
  response: google.protobuf.Empty
`

func TestParseAGoodCatalog(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Entries) != 2 || !c.Entries[0].Paginated || c.Entries[1].Paginated {
		t.Fatalf("entries = %+v", c.Entries)
	}
	eff := c.Effects()
	if eff["billing.invoice.list"] != EffectRead || eff["billing.invoice.void"] != EffectWrite || len(eff) != 2 {
		t.Fatalf("Effects = %v", eff)
	}
	me := c.MethodEffects()
	if me["/billing.v1.InvoiceService/ListInvoices"] != EffectRead || me["/billing.v1.InvoiceService/VoidInvoice"] != EffectWrite {
		t.Fatalf("MethodEffects = %v", me)
	}
	if r := c.Risks(); r["billing.invoice.void"] != RiskHigh {
		t.Fatalf("Risks = %v", r)
	}
	if e, ok := c.Lookup("billing.invoice.void"); !ok || e.Request != "billing.v1.VoidInvoiceRequest" {
		t.Fatalf("Lookup = %+v, %v", e, ok)
	}
	if _, ok := c.Lookup("billing.invoice.nope"); ok {
		t.Fatal("Lookup found an action that is not there")
	}
}

// Every defect is caught, and each is named in the error.
func TestParseRejects(t *testing.T) {
	entry := func(edit func(m map[string]string)) string {
		m := map[string]string{
			"id": "billing.invoice.list", "rpc": "billing.v1.InvoiceService/ListInvoices",
			"action": "billing.invoice.list", "effect": "read", "risk": "low",
			"request": "billing.v1.ListInvoicesRequest", "response": "billing.v1.ListInvoicesResponse",
		}
		edit(m)
		var b strings.Builder
		b.WriteString("- ")
		first := true
		for _, k := range []string{"id", "rpc", "action", "effect", "risk", "request", "response", "efect"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			if !first {
				b.WriteString("  ")
			}
			first = false
			b.WriteString(k + ": " + v + "\n")
		}
		return b.String()
	}
	cases := map[string]struct {
		yaml, want string
	}{
		"unknown field":      {entry(func(m map[string]string) { m["efect"] = "read" }), "efect"},
		"missing id":         {entry(func(m map[string]string) { delete(m, "id") }), "id is missing"},
		"id is not action":   {entry(func(m map[string]string) { m["id"] = "billing.invoices.list" }), "must equal action"},
		"id prefix no dot":   {entry(func(m map[string]string) { m["id"] = "billing.invoice.listx" }), "must equal action"},
		"action no verb":     {entry(func(m map[string]string) { m["id"] = "billing"; m["action"] = "billing" }), "has no verb"},
		"action empty seg":   {entry(func(m map[string]string) { m["id"] = "billing..list"; m["action"] = "billing..list" }), "empty or space"},
		"missing effect":     {entry(func(m map[string]string) { delete(m, "effect") }), "effect"},
		"bad effect":         {entry(func(m map[string]string) { m["effect"] = "Read" }), `effect "Read"`},
		"bad risk":           {entry(func(m map[string]string) { m["risk"] = "extreme" }), `risk "extreme"`},
		"missing risk":       {entry(func(m map[string]string) { delete(m, "risk") }), `risk ""`},
		"rpc without method": {entry(func(m map[string]string) { m["rpc"] = "billing.v1.InvoiceService" }), "rpc"},
		"rpc with slash":     {entry(func(m map[string]string) { m["rpc"] = "/billing.v1.InvoiceService/ListInvoices" }), "rpc"},
		"rpc no package":     {entry(func(m map[string]string) { m["rpc"] = "InvoiceService/ListInvoices" }), "rpc"},
		"short request":      {entry(func(m map[string]string) { m["request"] = "ListInvoicesRequest" }), "request"},
		"missing response":   {entry(func(m map[string]string) { delete(m, "response") }), "response"},
		"not a list":         {"id: x\n", "decode"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestParseRejectsDuplicates(t *testing.T) {
	dupID := good + `
- id: billing.invoice.list
  rpc: billing.v1.InvoiceService/ListInvoices2
  action: billing.invoice.list
  effect: read
  risk: low
  request: billing.v1.A
  response: billing.v1.B
`
	_, err := Parse([]byte(dupID))
	if err == nil || !strings.Contains(err.Error(), `id "billing.invoice.list" already listed by entry 1`) {
		t.Fatalf("duplicate id: %v", err)
	}
	dupRPC := good + `
- id: billing.invoice.other
  rpc: billing.v1.InvoiceService/ListInvoices
  action: billing.invoice.other
  effect: read
  risk: low
  request: billing.v1.A
  response: billing.v1.B
`
	_, err = Parse([]byte(dupRPC))
	if err == nil || !strings.Contains(err.Error(), `rpc "billing.v1.InvoiceService/ListInvoices" already listed`) {
		t.Fatalf("duplicate rpc: %v", err)
	}
}

// One action may cover several RPCs (actions are coarse); the action takes
// the most restrictive effect and the highest risk of its entries, in any
// order, and Lookup returns that entry.
func TestSharedActionTakesTheMostRestrictiveEffect(t *testing.T) {
	for _, order := range [][2]string{{"read", "write"}, {"write", "read"}} {
		c, err := Parse([]byte(`
- id: maintenance.list.list_orders
  rpc: maintenance.v1.S/ListOrders
  action: maintenance.list
  effect: ` + order[0] + `
  risk: low
  request: maintenance.v1.A
  response: maintenance.v1.B
- id: maintenance.list.list_plans
  rpc: maintenance.v1.S/ListPlans
  action: maintenance.list
  effect: ` + order[1] + `
  risk: high
  request: maintenance.v1.A
  response: maintenance.v1.B
`))
		if err != nil {
			t.Fatalf("%v: %v", order, err)
		}
		if got := c.Effects()["maintenance.list"]; got != EffectWrite {
			t.Fatalf("%v: effect = %q, want write", order, got)
		}
		if IsRead(c.Effects(), "maintenance.list") {
			t.Fatalf("%v: a shared action with one write entry is a read", order)
		}
		if got := c.Risks()["maintenance.list"]; got != RiskHigh {
			t.Fatalf("%v: risk = %q, want high", order, got)
		}
		if e, ok := c.Lookup("maintenance.list"); !ok || e.Effect != EffectWrite {
			t.Fatalf("%v: lookup = %+v %v", order, e, ok)
		}
		if got := c.MethodEffects()["/maintenance.v1.S/ListOrders"]; got != Effect(order[0]) {
			t.Fatalf("%v: method effect = %q, want the entry's own %q", order, got, order[0])
		}
	}
}

// An ungated RPC (empty action) is catalogued for its effect but never
// enters the action maps.
func TestUngatedEntry(t *testing.T) {
	c, err := Parse([]byte(`
- id: divisions.get_tree
  rpc: divisions.v1.DivisionsService/GetTree
  action: ""
  effect: read
  risk: low
  request: divisions.v1.GetTreeRequest
  response: divisions.v1.Tree
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Ungated()) != 1 || len(c.Effects()) != 0 || len(c.Risks()) != 0 {
		t.Fatalf("ungated=%d effects=%v risks=%v", len(c.Ungated()), c.Effects(), c.Risks())
	}
	if _, ok := c.Lookup(""); ok {
		t.Fatal("lookup of the empty action found an entry")
	}
	if c.MethodEffects()["/divisions.v1.DivisionsService/GetTree"] != EffectRead {
		t.Fatal("ungated entry missing from MethodEffects")
	}
	if err := c.CheckModules("divisions"); err != nil {
		t.Fatalf("CheckModules on an ungated entry: %v", err)
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Parse([]byte(`
- id: a.b
  rpc: x
  action: a.b
  effect: nope
  risk: low
  request: a.B
  response: a.C
`))
	if err == nil || strings.Count(err.Error(), "\n") < 1 {
		t.Fatalf("want several problems, one per line: %v", err)
	}
}

// An empty file is a catalog with nothing in it, under which nothing is a
// read; it is not a parse error.
func TestParseEmpty(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(empty) = %v", err)
	}
	if e := c.Effects(); e == nil || len(e) != 0 || IsRead(e, "a.read") {
		t.Fatalf("empty catalog effects = %v", e)
	}
}

func TestLoadAndMustParse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Fatal("Load of a missing file must fail")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustParse must panic on a bad catalog")
		}
	}()
	MustParse([]byte("- id: x\n"))
}

func TestCheckModules(t *testing.T) {
	c, _ := Parse([]byte(good))
	if err := c.CheckModules("billing"); err != nil {
		t.Fatalf("CheckModules(billing) = %v", err)
	}
	if err := c.CheckModules("invoices", "billing"); err != nil {
		t.Fatalf("CheckModules(invoices, billing) = %v", err)
	}
	if err := c.CheckModules("invoices"); err == nil || !strings.Contains(err.Error(), `module "billing"`) {
		t.Fatalf("CheckModules(invoices) = %v", err)
	}
	// The match is on the whole first segment, not a prefix.
	if err := c.CheckModules("bill"); err == nil {
		t.Fatal("a prefix of the module must not match")
	}
}

func TestCheckRPCs(t *testing.T) {
	c, _ := Parse([]byte(good))
	declared := []RPC{
		{"billing.v1.InvoiceService/ListInvoices", "billing.v1.ListInvoicesRequest", "billing.v1.ListInvoicesResponse"},
		{"billing.v1.InvoiceService/VoidInvoice", "billing.v1.VoidInvoiceRequest", "google.protobuf.Empty"},
	}
	if err := c.CheckRPCs(declared); err != nil {
		t.Fatalf("CheckRPCs = %v", err)
	}

	extra := append(append([]RPC(nil), declared...), RPC{"billing.v1.InvoiceService/DeleteInvoice", "billing.v1.A", "billing.v1.B"})
	if err := c.CheckRPCs(extra); err == nil || !strings.Contains(err.Error(), "rpc billing.v1.InvoiceService/DeleteInvoice is not catalogued") {
		t.Fatalf("uncatalogued rpc: %v", err)
	}
	if err := c.CheckRPCs(declared[:1]); err == nil || !strings.Contains(err.Error(), `"billing.v1.InvoiceService/VoidInvoice" is not declared`) {
		t.Fatalf("catalogued rpc that does not exist: %v", err)
	}
	wrong := append([]RPC(nil), declared...)
	wrong[0].Request = "billing.v1.Other"
	wrong[1].Response = "billing.v1.VoidInvoiceResponse"
	err := c.CheckRPCs(wrong)
	if err == nil || !strings.Contains(err.Error(), "takes \"billing.v1.Other\"") || !strings.Contains(err.Error(), "returns \"billing.v1.VoidInvoiceResponse\"") {
		t.Fatalf("request/response mismatch: %v", err)
	}
}
