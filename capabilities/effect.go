// Package capabilities reads a Core's operation catalog
// (capabilities/catalog.yaml, contract AI ready §1.4): one entry per public
// RPC, naming the action the gate authorizes it under and the EFFECT it has.
//
// The effect is what the platform classifies by. Whether an action is a read
// decides what the assistant may do (authz.Effects) and which calls need an
// idempotency key (package idempotency); it is the same classification in
// both places, read from the same file, so the two can never disagree about
// what a write is.
package capabilities

import "strings"

// Effect is what an operation does to the world.
type Effect string

// The effects a catalog entry may declare. Only EffectRead is a read; every
// other effect is a write for every purpose the kit has (the assistant may
// not perform it, a machine must send an idempotency key).
const (
	EffectRead               Effect = "read"
	EffectWrite              Effect = "write"
	EffectExternalSideEffect Effect = "external_side_effect"
	EffectDelete             Effect = "delete"
)

// Valid reports whether e is one of the four effects the contract defines.
func (e Effect) Valid() bool {
	switch e {
	case EffectRead, EffectWrite, EffectExternalSideEffect, EffectDelete:
		return true
	}
	return false
}

// IsRead reports whether action is a read.
//
// With a catalog (effects != nil) the catalog decides, and an action it does
// not list is NOT a read: an uncatalogued action is a gap in the catalog, and
// the safe reading of a gap is "may write". Without a catalog only the verbs
// read, list and get count (contract AI ready §1.3). That list is narrower on
// purpose than authz.ScopeFor's: lookup, search and view are reads for a
// service's core.read, but nothing guarantees a Core named a side-effecting
// action that way, and the assistant rule cannot afford the guess.
func IsRead(effects map[string]Effect, action string) bool {
	if effects != nil {
		e, ok := effects[action]
		return ok && e == EffectRead
	}
	verb := action[strings.LastIndex(action, ".")+1:]
	switch verb {
	case "read", "list", "get":
		return true
	}
	return false
}
