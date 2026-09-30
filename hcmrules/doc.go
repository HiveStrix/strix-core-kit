// Package hcmrules is the country-agnostic evaluation MECHANISM of the HCM
// rules engine: it turns effective-dated rule DATA (parameters, brackets,
// formulas, eligibility predicates, calendars, flags) into numbers,
// deterministically and without touching a database.
//
// It is pure computation — no I/O, no tenant state. The DATA lives in
// core-hcmrules (strix-hcm-rules): a consumer fetches the rules in force for
// (jurisdiction, region, date) ONCE (RulesService.GetRuleset) and evaluates
// locally instead of calling the service per formula.
//
//	EvalBracket(tiers, base) -> amount
//	EvalFormula(kind, coefficients, inputs) -> amount   // registry of pure funcs
//	CheckEligibility(kind, params, facts) -> bool
//	ResolveCalendar(entries, from, to) -> []Day
//	DecodeValues(json) -> coefficients/params as exact decimals
//
// formula_kind / predicate_kind are RESOLVED to registered Go functions, NOT
// interpreted from a free-form DSL: the calculation stays auditable — the same
// discipline core-costing uses for its explosion/margin functions. Amounts use
// exact decimals, never float.
//
// WHY IT LIVES IN THE KIT (ADR Hivestrix-gitops/docs/decisions/hcm-cores.md
// §2): core-hcmrules validates and previews with it, and core-leave,
// core-time and core-payroll evaluate with it. One copy, so the figure a
// tenant previews in the rules UI is the figure payroll pays. It started in
// strix-hcm-rules/pkg/rules and moved here when its second consumer
// (core-leave) arrived.
package hcmrules
