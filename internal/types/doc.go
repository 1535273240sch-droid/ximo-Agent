// Package types holds the cross-module contract types for ximo-agent.
//
// It is the single source of truth for every type that more than one of the
// seven module tasks both defines and consumes. Task 08 (integration)
// consolidates them here per chapter 31/32 step 3 so that no two module
// packages can drift apart, and so that no import cycle can form
// (e.g. task 04 depending on task 03's Event while task 03 imports task 04).
//
// Import rules — enforced by review, and part of the integration contract:
//
//  1. This package imports NOTHING from internal/. It is a leaf.
//  2. Module packages (internal/engine, internal/storage, ...) import
//     internal/types. Never the reverse.
//  3. A module package keeps ownership of its *service interfaces*
//     (Engine, EventStore, ToolRuntime, Worker, Provider, ...); only the
//     *data types* those interfaces traffic in live here.
//
// The adjudication record for conflicts found between the seven task briefs
// is in docs/接口裁决记录.md.
package types
