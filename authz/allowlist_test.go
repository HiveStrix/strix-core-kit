package authz

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
)

// machineCtx is the context a client_credentials token produces: sub ==
// client_id, no entitlements.
func machineCtx(clientID, scope string) context.Context {
	return auth.ContextWithClaims(context.Background(), &auth.Claims{
		Subject: clientID, ClientID: clientID, TenantID: "acme", Scope: scope,
	})
}

// peopleGate is the shape people needs: time and leave read employment
// records, only payroll reads compensation, and nothing else is open to a
// machine.
func peopleGate(t *testing.T, f *fakePDP) *Gate {
	t.Helper()
	g := newGate(t, f, "people")
	return New(g.pdp, "people",
		MachineAllow("people.employment_records.read", "core-time-m2m", "core-leave-m2m"),
		MachineAllow("people.compensation.read", "core-payroll-m2m"),
		MachineAllow("people.settlements.write", "core-payroll-m2m"),
	)
}

// Opt-in: a gate built without MachineAllow decides a machine by scope alone,
// as before v0.18.0, so no Core breaks when it bumps the kit.
func TestWithoutAllowlistAMachineIsDecidedByScopeAlone(t *testing.T) {
	f := &fakePDP{allowed: false}
	g := newGate(t, f, "people")

	for _, client := range []string{"core-time-m2m", "core-leave-m2m", "anything-m2m"} {
		if err := g.Require(machineCtx(client, "core.read"), "people.compensation.read", "employee", "e-1"); err != nil {
			t.Fatalf("%s: a gate with no allowlist must keep deciding by scope: %v", client, err)
		}
	}
	if err := g.Require(machineCtx("core-time-m2m", "core.read"), "people.compensation.write", "employee", "e-1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("core.read still cannot write: %v", err)
	}
	if _, calls := f.recorded(); calls != 0 {
		t.Fatalf("the PDP must never be asked about a machine: %d calls", calls)
	}
}

func TestAllowlistAdmitsOnlyTheClientsListedForThatExactAction(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := peopleGate(t, f)

	cases := []struct {
		client, action string
		allowed        bool
	}{
		{"core-time-m2m", "people.employment_records.read", true},
		{"core-leave-m2m", "people.employment_records.read", true},
		{"core-payroll-m2m", "people.compensation.read", true},

		// Listed, but for another action.
		{"core-time-m2m", "people.compensation.read", false},
		{"core-leave-m2m", "people.compensation.read", false},
		{"core-payroll-m2m", "people.employment_records.read", false},

		// The match is exact: a sibling or a longer name is another action.
		{"core-time-m2m", "people.employment_records.list", false},
		{"core-time-m2m", "people.employment_records.read.all", false},
		{"core-time-m2m", "people.employment_records", false},

		// An action nobody listed is closed to every machine.
		{"core-time-m2m", "people.dependents.read", false},
		{"core-payroll-m2m", "people.dependents.read", false},
		{"core-payroll-m2m", "people.employees.get", false},

		// A client nobody listed reaches nothing.
		{"core-costing-m2m", "people.compensation.read", false},
		{"core-costing-m2m", "people.employment_records.read", false},

		// client_id is compared exactly.
		{"CORE-TIME-M2M", "people.employment_records.read", false},
		{"core-time-m2m ", "people.employment_records.read", false},
	}
	for _, tc := range cases {
		err := g.Require(machineCtx(tc.client, "core.write"), tc.action, "employee", "e-1")
		if tc.allowed && err != nil {
			t.Errorf("%s %s: want allowed, got %v", tc.client, tc.action, err)
		}
		if !tc.allowed && status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s %s: want PermissionDenied, got %v", tc.client, tc.action, err)
		}
	}
	if _, calls := f.recorded(); calls != 0 {
		t.Fatalf("the PDP must never be asked about a machine: %d calls", calls)
	}
}

// Being listed narrows the scope, it does not replace it.
func TestAllowlistStillRequiresTheScope(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := peopleGate(t, f)

	if err := g.Require(machineCtx("core-payroll-m2m", "core.read"), "people.settlements.write", "settlement", "s-1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("listed with core.read must not write: %v", err)
	}
	if err := g.Require(machineCtx("core-payroll-m2m", "core.write"), "people.settlements.write", "settlement", "s-1"); err != nil {
		t.Fatalf("listed with core.write must write: %v", err)
	}
	if err := g.Require(machineCtx("core-payroll-m2m", ""), "people.compensation.read", "employee", "e-1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("listed with no scope must not read: %v", err)
	}
	if err := g.Require(machineCtx("core-payroll-m2m", "core.read"), "people.compensation.read", "employee", "e-1"); err != nil {
		t.Fatalf("listed with core.read must read: %v", err)
	}
}

// People never go through the allowlist: entitlement and the PDP decide, the
// same with or without one.
func TestAllowlistLeavesPeopleToThePDP(t *testing.T) {
	f := &fakePDP{allowed: true}
	g := peopleGate(t, f)

	if err := g.Require(ctxWith("people"), "people.dependents.read", "dependent", "d-1"); err != nil {
		t.Fatalf("a person allowed by the PDP must pass an action no machine is listed for: %v", err)
	}
	if _, calls := f.recorded(); calls != 1 {
		t.Fatalf("the PDP must decide the person: %d calls, want 1", calls)
	}

	// A user token minted through a listed client is still a person: its sub
	// is the user, not the client.
	user := auth.ContextWithClaims(context.Background(), &auth.Claims{
		Subject: "user-1", ClientID: "core-payroll-m2m", TenantID: "acme", Entitlements: []string{"people"},
	})
	if err := g.Require(user, "people.dependents.read", "dependent", "d-1"); err != nil {
		t.Fatalf("a person must not be judged by the client's allowlist: %v", err)
	}

	deny := &fakePDP{allowed: false}
	gd := peopleGate(t, deny)
	if err := gd.Require(ctxWith("people"), "people.compensation.read", "employee", "e-1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a person the PDP denies stays denied, even on an action a machine is listed for: %v", err)
	}
	if err := gd.Require(ctxWith("costing"), "people.compensation.read", "employee", "e-1"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unentitled tenant stays denied: %v", err)
	}
}

// The denial an unlisted machine gets says nothing about who IS listed, nor
// whether the action has an allowlist entry at all: it is the same text the
// PDP's denial uses.
func TestAllowlistDenialRevealsNothingAboutOtherClients(t *testing.T) {
	f := &fakePDP{allowed: false}
	g := peopleGate(t, f)

	messages := map[string]string{}
	for _, action := range []string{
		"people.compensation.read",       // listed for another client
		"people.employment_records.read", // listed for two other clients
		"people.dependents.read",         // listed for nobody
		"people.does_not_exist.read",     // not an action at all
	} {
		err := g.Require(machineCtx("core-costing-m2m", "core.write"), action, "x", "")
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s: code = %v, want PermissionDenied", action, status.Code(err))
		}
		msg := status.Convert(err).Message()
		for _, leaked := range []string{"core-time-m2m", "core-leave-m2m", "core-payroll-m2m", "core-costing-m2m", "allowlist", "listed", action} {
			if strings.Contains(msg, leaked) {
				t.Errorf("%s: the denial %q reveals %q", action, msg, leaked)
			}
		}
		messages[msg] = action
	}
	if len(messages) != 1 {
		t.Fatalf("an unlisted machine must get one and the same denial for every action, got %v", messages)
	}

	pdpDenial := g.Require(ctxWith("people"), "people.compensation.read", "employee", "e-1")
	if _, same := messages[status.Convert(pdpDenial).Message()]; !same {
		t.Fatalf("the allowlist denial must read like the PDP's (%q), got %v", status.Convert(pdpDenial).Message(), messages)
	}
}

// A listed machine without the scope must not be told apart from an
// unlisted one: naming the missing scope would answer "yes, you are on the
// allowlist for this action".
func TestAllowlistScopeDenialIsIndistinguishableFromNotListed(t *testing.T) {
	f := &fakePDP{allowed: false}
	g := peopleGate(t, f)

	listed := g.Require(machineCtx("core-payroll-m2m", "core.read"), "people.settlements.write", "settlement", "s-1")
	unlisted := g.Require(machineCtx("core-time-m2m", "core.read"), "people.settlements.write", "settlement", "s-1")
	noScope := g.Require(machineCtx("core-payroll-m2m", ""), "people.compensation.read", "employee", "e-1")
	for name, err := range map[string]error{"listed, core.read": listed, "unlisted, core.read": unlisted, "listed, no scope": noScope} {
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s: code = %v, want PermissionDenied", name, status.Code(err))
		}
		if msg := status.Convert(err).Message(); msg != "authz: denied" {
			t.Errorf("%s: message %q, want the plain \"authz: denied\"", name, msg)
		}
	}

	// Without an allowlist there is nothing to hide, and the message keeps
	// naming the scope as before v0.18.0.
	plain := newGate(t, f, "people")
	err := plain.Require(machineCtx("core-payroll-m2m", "core.read"), "people.settlements.write", "settlement", "s-1")
	if msg := status.Convert(err).Message(); !strings.Contains(msg, `lacks scope "core.write"`) {
		t.Errorf("a gate without allowlist must keep its message, got %q", msg)
	}
}

// Two MachineAllow for the same action add up, and the list is copied: a
// slice the caller reuses afterwards cannot widen or narrow the gate.
func TestAllowlistEntriesAddUpAndAreCopied(t *testing.T) {
	f := &fakePDP{allowed: true}
	base := newGate(t, f, "people")

	ids := []string{"core-time-m2m"}
	opt := MachineAllow("people.employment_records.read", ids...)
	ids[0] = "core-costing-m2m"
	g := New(base.pdp, "people", opt,
		MachineAllow("people.employment_records.read", "core-leave-m2m", "core-leave-m2m"),
	)

	for _, client := range []string{"core-time-m2m", "core-leave-m2m"} {
		if err := g.Require(machineCtx(client, "core.read"), "people.employment_records.read", "employee", ""); err != nil {
			t.Errorf("%s: want allowed, got %v", client, err)
		}
	}
	if err := g.Require(machineCtx("core-costing-m2m", "core.read"), "people.employment_records.read", "employee", ""); status.Code(err) != codes.PermissionDenied {
		t.Errorf("mutating the caller's slice must not add a client: %v", err)
	}
}

// A malformed allowlist is a programming error in the Core's own code, and it
// stops the process when the gate is built instead of shipping a gate that
// silently denies — or admits — someone nobody meant to.
func TestAllowlistProgrammingErrorsFailAtConstruction(t *testing.T) {
	cases := map[string]Option{
		"action of another module":   MachineAllow("time.periods.read", "core-payroll-m2m"),
		"module that only prefixes":  MachineAllow("peoplex.compensation.read", "core-payroll-m2m"),
		"action with no verb":        MachineAllow("people", "core-payroll-m2m"),
		"empty action":               MachineAllow("", "core-payroll-m2m"),
		"empty segment":              MachineAllow("people..read", "core-payroll-m2m"),
		"trailing dot":               MachineAllow("people.compensation.", "core-payroll-m2m"),
		"space in the action":        MachineAllow("people.compensation.read ", "core-payroll-m2m"),
		"no client at all":           MachineAllow("people.compensation.read"),
		"empty client_id":            MachineAllow("people.compensation.read", ""),
		"empty among valid ones":     MachineAllow("people.compensation.read", "core-payroll-m2m", ""),
		"padded client_id":           MachineAllow("people.compensation.read", " core-payroll-m2m"),
		"client_id with a tab":       MachineAllow("people.compensation.read", "core-payroll-m2m\t"),
		"client_id with inner space": MachineAllow("people.compensation.read", "core payroll"),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			msg := panicMessage(func() { New(nil, "people", opt) })
			if msg == "" {
				t.Fatal("New must panic on a malformed allowlist")
			}
			if !strings.HasPrefix(msg, "authz: MachineAllow") {
				t.Errorf("panic %q does not say where it came from", msg)
			}
		})
	}

	// And a well-formed one does not.
	if msg := panicMessage(func() {
		New(nil, "people", MachineAllow("people.compensation.read", "core-payroll-m2m"))
	}); msg != "" {
		t.Fatalf("a valid allowlist must build: %s", msg)
	}
}

func panicMessage(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	fn()
	return ""
}
