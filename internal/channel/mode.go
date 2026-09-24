package channel

import "sync/atomic"

// ChannelMode is the lifecycle state of a running channel.
type ChannelMode int32

const (
	ModeLive            ChannelMode = 0
	ModeTransitioning   ChannelMode = 1
	ModeVOD             ChannelMode = 2
	ModeStaticIngesting ChannelMode = 3 // static MPD discovered; segments downloading
	ModeVODFailed       ChannelMode = 4 // ingest stopped; the VOD transition failed and may be retried
)

// String returns the mode as the status API reports it.
func (m ChannelMode) String() string {
	switch m {
	case ModeTransitioning:
		return "transitioning"
	case ModeVOD:
		return "vod"
	case ModeStaticIngesting:
		return "static_ingesting"
	case ModeVODFailed:
		return "vod_failed"
	}
	return "live"
}

// claimTrigger says who is asking to start the VOD transition.
type claimTrigger int

const (
	// claimAuto is the channel itself, after its source completed and the
	// admitted work drained.
	claimAuto claimTrigger = iota
	// claimManual is an operator. It may also retry a failed transition, and
	// it overrides a restore's auto-VOD suppression.
	claimManual
)

// modeTransition names one legal change of Channel mode.
type modeTransition int

const (
	transitionPromoteStatic modeTransition = iota
	transitionClaimAuto
	transitionClaimManual
	transitionFail
	transitionComplete
)

// modeTransitions is every legal change of Channel mode: for each transition,
// the modes it may start from and the mode it leads to. Anything missing here
// is refused. In particular only an operator retries a failed transition — the
// channel never does so on its own — and nothing leaves VOD: restoring a VOD
// channel to live restarts it, which builds a new runtime.
var modeTransitions = map[modeTransition]struct {
	from []ChannelMode
	to   ChannelMode
}{
	transitionPromoteStatic: {[]ChannelMode{ModeLive}, ModeStaticIngesting},
	transitionClaimAuto:     {[]ChannelMode{ModeStaticIngesting}, ModeTransitioning},
	transitionClaimManual:   {[]ChannelMode{ModeLive, ModeStaticIngesting, ModeVODFailed}, ModeTransitioning},
	transitionFail:          {[]ChannelMode{ModeTransitioning}, ModeVODFailed},
	transitionComplete:      {[]ChannelMode{ModeTransitioning}, ModeVOD},
}

// channelModeMachine is the only writer of a runtime's Channel mode and of its
// auto-VOD suppression. Every change goes through a named transition, which
// either happens atomically or is refused and reports the mode that refused
// it; there is no way to store a mode directly.
//
// Its zero value is a live channel whose auto-VOD is allowed. Use begin to
// start elsewhere. It must not be copied after first use.
//
// The suppression lives here because it is a rule about the mode: it decides
// whether the channel may leave Live on its own. A channel restored to live
// would otherwise fall straight back into VOD, since its upstream is normally
// still finished; the suppression holds it until the upstream produces a new
// segment, or an operator asks for the transition by hand.
type channelModeMachine struct {
	mode atomic.Int32 // ChannelMode
	// suppressed blocks the automatic VOD transition. baseline is the
	// stored-segment count when the suppression was installed; it lifts as soon
	// as the count exceeds it.
	suppressed atomic.Bool
	baseline   atomic.Uint64
}

// begin sets the state a runtime starts in. It is called once, before the
// runtime is published: initial is ModeLive, or ModeVOD for a channel
// restarting from a completed transition. suppress installs the auto-VOD
// suppression with baseline as the stored-segment count to exceed.
func (c *channelModeMachine) begin(initial ChannelMode, suppress bool, baseline uint64) {
	c.mode.Store(int32(initial))
	c.suppressed.Store(suppress)
	c.baseline.Store(baseline)
}

// Mode returns the current Channel mode.
func (c *channelModeMachine) Mode() ChannelMode { return ChannelMode(c.mode.Load()) }

// PromoteStatic moves a live channel to static ingesting, once its upstream
// has shown itself to be a finished asset.
func (c *channelModeMachine) PromoteStatic() (from ChannelMode, ok bool) {
	return c.apply(transitionPromoteStatic)
}

// Claim starts the VOD transition. Exactly one of any number of concurrent
// claims succeeds, which is what gives an automatic completion racing an
// operator's request a single winner.
//
// A successful manual claim also lifts the auto-VOD suppression: an operator
// asking for the transition wants it regardless of a restore.
func (c *channelModeMachine) Claim(by claimTrigger) (from ChannelMode, ok bool) {
	t := transitionClaimAuto
	if by == claimManual {
		t = transitionClaimManual
	}
	from, ok = c.apply(t)
	if ok && by == claimManual {
		c.suppressed.Store(false)
	}
	return from, ok
}

// Fail ends a claimed transition that could not produce a VOD. The channel's
// ingest has already stopped; an operator may claim it again to retry.
func (c *channelModeMachine) Fail() (from ChannelMode, ok bool) { return c.apply(transitionFail) }

// Complete ends a claimed transition in VOD.
func (c *channelModeMachine) Complete() (from ChannelMode, ok bool) {
	return c.apply(transitionComplete)
}

// AutoAllowed reports whether the channel may transition to VOD on its own,
// given how many segments it has stored so far. Once stored exceeds the
// suppression's baseline the suppression lifts for good.
//
// It asks for a count rather than a notification from the processor because
// the stored count is already the one place that means "a new segment was
// committed"; a second signal would be a second thing to keep true.
func (c *channelModeMachine) AutoAllowed(stored uint64) bool {
	if !c.suppressed.Load() {
		return true
	}
	if stored > c.baseline.Load() {
		c.suppressed.Store(false)
		return true
	}
	return false
}

// apply performs t if the current mode allows it. The compare-and-swap loop
// makes the check and the change one atomic step, and from is the mode the
// decision was made against.
func (c *channelModeMachine) apply(t modeTransition) (from ChannelMode, ok bool) {
	rule := modeTransitions[t]
	for {
		from = c.Mode()
		if !allowedFrom(rule.from, from) {
			return from, false
		}
		if c.mode.CompareAndSwap(int32(from), int32(rule.to)) {
			return from, true
		}
	}
}

func allowedFrom(allowed []ChannelMode, m ChannelMode) bool {
	for _, a := range allowed {
		if a == m {
			return true
		}
	}
	return false
}
