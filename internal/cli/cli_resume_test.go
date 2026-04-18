package cli

// Tests for the resume path live alongside resolveIndexModels now — the
// [C]/[R]/[Q] three-choice dialog was removed in favour of a unified
// multi-select where marker models come back pre-checked. The new
// contract is covered by discover_test.go:
//
//   - TestResolveIndexModels_TTYPreSelectsConfigAndMarker — marker models
//     show up pre-selected in the prompt with the "in progress" annotation.
//   - TestResolveIndexModels_TTYPreSelectConflictPrefersMarkerAnnotation —
//     when a model is both configured and in-progress, the marker
//     annotation wins.
//   - TestResolveIndexModels_NonTTYMarkerAutoContinues — non-TTY still
//     auto-continues with the marker's recorded models, no prompt.
//   - TestResolveIndexModels_ZeroSelectedExitsCleanly — submitting the
//     prompt with zero selections is a clean no-op exit; the caller
//     preserves the marker on disk.
//
// The only runtime symbol left in cli_resume.go is isStdinTTY, a thin
// wrapper around os.Stdin's file mode used by cliIndex at call time. That
// helper is trivial and platform-specific; exercising it in unit tests
// would just test the standard library.
