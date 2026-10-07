package capabilities

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// Risk is how much harm a mistaken call can do. It is metadata for the AI
// Core (what to confirm with a person, what never to automate); the kit does
// not authorize on it.
type Risk string

// The risks a catalog entry may declare.
const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

// Valid reports whether r is one of the four risks the contract defines.
func (r Risk) Valid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	}
	return false
}

// Entry is one operation of the catalog: one public RPC.
type Entry struct {
	// ID identifies the entry. It equals Action, or is Action plus a suffix
	// ("<action>.<rpc>") when one action covers several RPCs: actions are
	// coarse in practice (maintenance.list covers two dozen RPCs), so the id,
	// not the action, is what is unique (amendment to contract AI ready §1.4,
	// 2026-10-06). An ungated entry (Action empty) may use any id.
	ID string `yaml:"id"`
	// RPC is "<package>.<Service>/<Method>", e.g.
	// "billing.v1.InvoiceService/ListInvoices".
	RPC string `yaml:"rpc"`
	// Action is the name the handler passes to authz.Gate.Require. Empty
	// means the RPC does not call the gate at all (claims only): it is
	// catalogued so its effect is known to interceptors, but it never enters
	// Effects, and cmd/capcheck lists it as a warning, because nothing but
	// the token decides who may call it.
	Action    string `yaml:"action"`
	Effect    Effect `yaml:"effect"`
	Risk      Risk   `yaml:"risk"`
	Paginated bool   `yaml:"paginated"`
	// Request and Response are the fully qualified message names of the RPC,
	// e.g. "billing.v1.ListInvoicesRequest".
	Request  string `yaml:"request"`
	Response string `yaml:"response"`
}

// FullMethod is the RPC as gRPC names it to an interceptor:
// "/billing.v1.InvoiceService/ListInvoices".
func (e Entry) FullMethod() string {
	return "/" + e.RPC
}

// Catalog is a parsed and validated operation catalog.
type Catalog struct {
	Entries []Entry
}

// Load reads and validates a catalog file.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("capabilities: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("capabilities: %s: %w", path, errors.Unwrap(err))
	}
	return c, nil
}

// MustParse is Parse for a catalog embedded in the Core's binary
// (go:embed): a malformed catalog is a build mistake, and it stops the
// process at startup like regexp.MustCompile instead of shipping a gate that
// classifies by a half-read file.
func MustParse(data []byte) *Catalog {
	c, err := Parse(data)
	if err != nil {
		panic(err)
	}
	return c
}

var (
	rpcRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+/[A-Za-z_][A-Za-z0-9_]*$`)
	messageRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
)

// Parse decodes and validates a catalog. Unknown fields are an error (a
// misspelt "efect" must not quietly leave an entry without an effect), and so
// is every missing or out-of-enum field, a malformed rpc or message name, an
// id that is neither its action nor its action plus a suffix, and an rpc or
// id listed twice (an action may repeat: it can cover several RPCs). All the
// problems are reported together, one per line.
func Parse(data []byte) (*Catalog, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var entries []Entry
	if err := dec.Decode(&entries); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF is an empty file: a catalog with no entries, valid, under
		// which nothing is a read.
		return nil, fmt.Errorf("capabilities: decode: %w", err)
	}
	c := &Catalog{Entries: entries}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("capabilities: %w", err)
	}
	return c, nil
}

func (c *Catalog) validate() error {
	var problems []string
	add := func(i int, e Entry, format string, args ...any) {
		name := e.ID
		if name == "" {
			name = e.RPC
		}
		problems = append(problems, fmt.Sprintf("entry %d (%s): %s", i+1, name, fmt.Sprintf(format, args...)))
	}
	seenID, seenRPC := map[string]int{}, map[string]int{}
	for i, e := range c.Entries {
		if e.ID == "" {
			add(i, e, "id is missing")
		}
		if e.Action != "" {
			if err := checkActionName(e.Action); err != nil {
				add(i, e, "%v", err)
			}
			if e.ID != "" && e.ID != e.Action && !strings.HasPrefix(e.ID, e.Action+".") {
				add(i, e, "id %q must equal action %q or start with %q", e.ID, e.Action, e.Action+".")
			}
		}
		if !rpcRe.MatchString(e.RPC) {
			add(i, e, "rpc %q is not \"<package>.<Service>/<Method>\"", e.RPC)
		}
		if !e.Effect.Valid() {
			add(i, e, "effect %q is not one of read, write, external_side_effect, delete", e.Effect)
		}
		if !e.Risk.Valid() {
			add(i, e, "risk %q is not one of low, medium, high, critical", e.Risk)
		}
		if !messageRe.MatchString(e.Request) {
			add(i, e, "request %q is not a fully qualified message name", e.Request)
		}
		if !messageRe.MatchString(e.Response) {
			add(i, e, "response %q is not a fully qualified message name", e.Response)
		}
		for _, d := range []struct {
			seen  map[string]int
			key   string
			field string
		}{{seenID, e.ID, "id"}, {seenRPC, e.RPC, "rpc"}} {
			if d.key == "" {
				continue
			}
			if first, dup := d.seen[d.key]; dup {
				add(i, e, "%s %q already listed by entry %d", d.field, d.key, first+1)
			} else {
				d.seen[d.key] = i
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// checkActionName is the shape authz requires: "<module>[.<group>...].<verb>",
// no empty or space-carrying segment.
func checkActionName(action string) error {
	segments := strings.Split(action, ".")
	if len(segments) < 2 {
		return fmt.Errorf("action %q has no verb; want \"<module>[.<group>...].<verb>\"", action)
	}
	for _, s := range segments {
		if s == "" || strings.IndexFunc(s, unicode.IsSpace) >= 0 {
			return fmt.Errorf("action %q has an empty or space-carrying segment", action)
		}
	}
	return nil
}

// CheckModules reports every entry whose action does not start with one of
// modules (its first segment). A Core authorizes under its own modules only;
// an action of another module would be judged by that module's rules.
func (c *Catalog) CheckModules(modules ...string) error {
	allowed := map[string]bool{}
	for _, m := range modules {
		allowed[m] = true
	}
	var problems []string
	for i, e := range c.Entries {
		if e.Action == "" {
			continue
		}
		mod, _, _ := strings.Cut(e.Action, ".")
		if !allowed[mod] {
			problems = append(problems, fmt.Sprintf("entry %d (%s): action module %q is not one of %v", i+1, e.ID, mod, modules))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// Effects is action -> effect, what authz.Effects takes. When one action
// covers several RPCs with different effects, the action gets the most
// restrictive of them (delete over external_side_effect over write over
// read): the gate sees the action, not the RPC, so an action that is a read
// for one RPC and a write for another is a write. Ungated entries are left
// out: no Require call ever names them.
func (c *Catalog) Effects() map[string]Effect {
	out := make(map[string]Effect, len(c.Entries))
	for _, e := range c.Entries {
		if e.Action == "" {
			continue
		}
		if cur, ok := out[e.Action]; !ok || effectRank(e.Effect) > effectRank(cur) {
			out[e.Action] = e.Effect
		}
	}
	return out
}

// effectRank orders effects from least to most restrictive.
func effectRank(e Effect) int {
	switch e {
	case EffectRead:
		return 0
	case EffectWrite:
		return 1
	case EffectExternalSideEffect:
		return 2
	case EffectDelete:
		return 3
	}
	return 4 // unknown: treated as the most restrictive
}

// Ungated returns the entries whose RPC does not call the gate.
func (c *Catalog) Ungated() []Entry {
	var out []Entry
	for _, e := range c.Entries {
		if e.Action == "" {
			out = append(out, e)
		}
	}
	return out
}

// MethodEffects is gRPC full method ("/pkg.Service/Method") -> effect, what
// an interceptor sees before any handler names the action
// (idempotency.Methods takes it).
func (c *Catalog) MethodEffects() map[string]Effect {
	out := make(map[string]Effect, len(c.Entries))
	for _, e := range c.Entries {
		out[e.FullMethod()] = e.Effect
	}
	return out
}

// Risks is action -> risk, the highest among the action's entries.
func (c *Catalog) Risks() map[string]Risk {
	out := make(map[string]Risk, len(c.Entries))
	for _, e := range c.Entries {
		if e.Action == "" {
			continue
		}
		if cur, ok := out[e.Action]; !ok || riskRank(e.Risk) > riskRank(cur) {
			out[e.Action] = e.Risk
		}
	}
	return out
}

func riskRank(r Risk) int {
	switch r {
	case RiskLow:
		return 0
	case RiskMedium:
		return 1
	case RiskHigh:
		return 2
	}
	return 3 // critical, or unknown
}

// Lookup returns the most restrictive entry of an action (see Effects).
func (c *Catalog) Lookup(action string) (Entry, bool) {
	var best Entry
	found := false
	for _, e := range c.Entries {
		if e.Action == action && action != "" && (!found || effectRank(e.Effect) > effectRank(best.Effect)) {
			best, found = e, true
		}
	}
	return best, found
}

// RPC is one method a Core's proto declares, as cmd/capcheck reads it.
type RPC struct {
	// Name is "<package>.<Service>/<Method>".
	Name string
	// Request and Response are fully qualified message names.
	Request  string
	Response string
}

// CheckRPCs compares the catalog against the RPCs the Core's protos declare
// and reports, together: every RPC the catalog does not list (contract AI
// ready §1.4: every public RPC is catalogued), every entry whose rpc does not
// exist, and every entry whose request or response is not the RPC's.
func (c *Catalog) CheckRPCs(rpcs []RPC) error {
	declared := make(map[string]RPC, len(rpcs))
	for _, r := range rpcs {
		declared[r.Name] = r
	}
	listed := make(map[string]bool, len(c.Entries))
	var problems []string
	for i, e := range c.Entries {
		listed[e.RPC] = true
		r, ok := declared[e.RPC]
		if !ok {
			problems = append(problems, fmt.Sprintf("entry %d (%s): rpc %q is not declared by any proto", i+1, e.ID, e.RPC))
			continue
		}
		if e.Request != r.Request {
			problems = append(problems, fmt.Sprintf("entry %d (%s): request %q, but %s takes %q", i+1, e.ID, e.Request, e.RPC, r.Request))
		}
		if e.Response != r.Response {
			problems = append(problems, fmt.Sprintf("entry %d (%s): response %q, but %s returns %q", i+1, e.ID, e.Response, e.RPC, r.Response))
		}
	}
	for _, r := range rpcs {
		if !listed[r.Name] {
			problems = append(problems, fmt.Sprintf("rpc %s is not catalogued", r.Name))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}
