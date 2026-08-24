package wasm

import "time"

// PythonReactorPhase identifies one measured Python Reactor lifecycle boundary.
type PythonReactorPhase string

const (
	PythonReactorPhaseArtifactVerify PythonReactorPhase = "artifact-verify"
	PythonReactorPhaseRuntimeCreate  PythonReactorPhase = "runtime-create"
	PythonReactorPhaseWASIImports    PythonReactorPhase = "wasi-imports"
	PythonReactorPhaseCompile        PythonReactorPhase = "compile"
	PythonReactorPhaseInstantiate    PythonReactorPhase = "instantiate"
	PythonReactorPhaseInitialize     PythonReactorPhase = "initialize"
	PythonReactorPhaseRuntimeInit    PythonReactorPhase = "runtime-init"
	PythonReactorPhaseRuntimePrepare PythonReactorPhase = "runtime-prepare"
	PythonReactorPhaseHeadroom       PythonReactorPhase = "headroom"
	PythonReactorPhaseStrategySelect PythonReactorPhase = "strategy-select"
	PythonReactorPhaseSnapshotTake   PythonReactorPhase = "snapshot-take"
	PythonReactorPhaseCheckout       PythonReactorPhase = "checkout"
	PythonReactorPhaseExecute        PythonReactorPhase = "execute"
	PythonReactorPhaseDecode         PythonReactorPhase = "decode"
	PythonReactorPhaseRestore        PythonReactorPhase = "restore"
	PythonReactorPhaseClose          PythonReactorPhase = "close"
)

// PythonReactorPurpose explains why a slot or phase was created.
type PythonReactorPurpose string

const (
	PythonReactorPurposeStartup     PythonReactorPurpose = "startup"
	PythonReactorPurposeRequest     PythonReactorPurpose = "request"
	PythonReactorPurposeFresh       PythonReactorPurpose = "fresh"
	PythonReactorPurposeRefill      PythonReactorPurpose = "refill"
	PythonReactorPurposeReplacement PythonReactorPurpose = "replacement"
)

// PythonReactorOutcome is the terminal state of one observed phase.
type PythonReactorOutcome string

const (
	PythonReactorOutcomeOK    PythonReactorOutcome = "ok"
	PythonReactorOutcomeError PythonReactorOutcome = "error"
)

func pythonReactorPhaseOutcome(err error) PythonReactorOutcome {
	if err != nil {
		return PythonReactorOutcomeError
	}
	return PythonReactorOutcomeOK
}

// PythonReactorPhaseEvent is immutable phase evidence delivered after timing stops.
// Observer callbacks may be concurrent during single-use refill.
type PythonReactorPhaseEvent struct {
	Phase             PythonReactorPhase   `json:"phase"`
	Purpose           PythonReactorPurpose `json:"purpose,omitempty"`
	Lifecycle         string               `json:"lifecycle,omitempty"`
	SnapshotRequested string               `json:"snapshot_requested,omitempty"`
	SnapshotSelected  string               `json:"snapshot_selected,omitempty"`
	RequestID         uint64               `json:"request_id,omitempty"`
	SlotID            uint64               `json:"slot_id,omitempty"`
	Duration          time.Duration        `json:"duration_ns"`
	MemoryBytes       uint64               `json:"memory_bytes,omitempty"`
	Outcome           PythonReactorOutcome `json:"outcome"`
	Error             string               `json:"error,omitempty"`
}

// PythonReactorPhaseObservation is the internal input used to finish a phase.
type PythonReactorPhaseObservation struct {
	Phase            PythonReactorPhase
	Purpose          PythonReactorPurpose
	RequestID        uint64
	SlotID           uint64
	Started          time.Time
	MemoryBytes      uint64
	SnapshotSelected string
	Outcome          PythonReactorOutcome
	Err              error
}

func (d *PythonReactorDispatcher) observePythonReactorPhase(observation PythonReactorPhaseObservation) {
	observer := d.cfg.PythonReactorObserver
	if observer == nil {
		return
	}
	duration := time.Duration(0)
	if !observation.Started.IsZero() {
		duration = time.Since(observation.Started)
	}
	event := PythonReactorPhaseEvent{
		Phase:             observation.Phase,
		Purpose:           observation.Purpose,
		Lifecycle:         d.cfg.PythonLifecycle,
		SnapshotRequested: d.snapshotMode(),
		SnapshotSelected:  observation.SnapshotSelected,
		RequestID:         observation.RequestID,
		SlotID:            observation.SlotID,
		Duration:          duration,
		MemoryBytes:       observation.MemoryBytes,
		Outcome:           observation.Outcome,
	}
	if observation.Err != nil {
		event.Error = observation.Err.Error()
	}
	d.emitPythonReactorPhaseEvent(observer, event)
}

func (d *PythonReactorDispatcher) emitPythonReactorPhaseEvent(observer func(PythonReactorPhaseEvent), event PythonReactorPhaseEvent) {
	if observer == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			d.log.Warn("python-reactor observer panicked")
		}
	}()
	observer(event)
}
