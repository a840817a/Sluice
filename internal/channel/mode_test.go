package channel

import (
	"sync"
	"sync/atomic"
	"testing"
)

// driveMode moves rt from its current mode to want through legal transitions
// only, so a test cannot set up a state the channel could never reach.
func driveMode(t *testing.T, rt *Runtime, want ChannelMode) {
	t.Helper()
	step := func(name string, f func() (ChannelMode, bool)) {
		t.Helper()
		if from, ok := f(); !ok {
			t.Fatalf("driveMode(%s): %s refused from %s", want, name, from)
		}
	}
	claim := func() (ChannelMode, bool) { return rt.modes.Claim(claimManual) }
	switch want {
	case ModeLive:
		if got := rt.Mode(); got != ModeLive {
			t.Fatalf("driveMode(live): nothing leads back to live from %s", got)
		}
	case ModeStaticIngesting:
		step("PromoteStatic", rt.modes.PromoteStatic)
	case ModeTransitioning:
		step("Claim", claim)
	case ModeVODFailed:
		step("Claim", claim)
		step("Fail", rt.modes.Fail)
	case ModeVOD:
		step("Claim", claim)
		step("Complete", rt.modes.Complete)
	}
}

var allModes = []ChannelMode{ModeLive, ModeStaticIngesting, ModeTransitioning, ModeVOD, ModeVODFailed}

// TestChannelModeTransitions pins every (mode, transition) pair. The expected
// table is written out here rather than read from modeTransitions, so a change
// to the rules has to change both.
func TestChannelModeTransitions(t *testing.T) {
	type op struct {
		name string
		do   func(c *channelModeMachine) (ChannelMode, bool)
	}
	ops := []op{
		{"PromoteStatic", (*channelModeMachine).PromoteStatic},
		{"Claim(auto)", func(c *channelModeMachine) (ChannelMode, bool) { return c.Claim(claimAuto) }},
		{"Claim(manual)", func(c *channelModeMachine) (ChannelMode, bool) { return c.Claim(claimManual) }},
		{"Fail", (*channelModeMachine).Fail},
		{"Complete", (*channelModeMachine).Complete},
	}
	// legal[op][from] is the mode op leads to; a missing entry is a refusal.
	legal := map[string]map[ChannelMode]ChannelMode{
		"PromoteStatic": {ModeLive: ModeStaticIngesting},
		"Claim(auto)":   {ModeStaticIngesting: ModeTransitioning},
		"Claim(manual)": {
			ModeLive:            ModeTransitioning,
			ModeStaticIngesting: ModeTransitioning,
			ModeVODFailed:       ModeTransitioning,
		},
		"Fail":     {ModeTransitioning: ModeVODFailed},
		"Complete": {ModeTransitioning: ModeVOD},
	}

	for _, o := range ops {
		for _, from := range allModes {
			t.Run(o.name+" from "+from.String(), func(t *testing.T) {
				var c channelModeMachine
				c.mode.Store(int32(from))

				gotFrom, ok := o.do(&c)

				to, want := legal[o.name][from]
				if gotFrom != from {
					t.Errorf("reported from = %s, want %s", gotFrom, from)
				}
				if ok != want {
					t.Fatalf("ok = %v, want %v", ok, want)
				}
				if !want {
					to = from
				}
				if got := c.Mode(); got != to {
					t.Errorf("mode = %s, want %s", got, to)
				}
			})
		}
	}
}

func TestChannelModeZeroValueIsLive(t *testing.T) {
	var c channelModeMachine
	if c.Mode() != ModeLive {
		t.Errorf("zero value mode = %s, want live", c.Mode())
	}
	if !c.AutoAllowed(0) {
		t.Error("zero value suppresses auto-VOD")
	}
}

// A channel restarted from a completed transition begins in VOD.
func TestChannelModeBeginsInVOD(t *testing.T) {
	var c channelModeMachine
	c.begin(ModeVOD, false, 0)
	if c.Mode() != ModeVOD {
		t.Fatalf("mode = %s, want vod", c.Mode())
	}
	if _, ok := c.Claim(claimManual); ok {
		t.Error("a VOD channel accepted a new transition")
	}
}

// Automatic completion and any number of operator requests can race for the
// same channel; exactly one may run the transition.
func TestChannelModeClaimHasOneWinner(t *testing.T) {
	for i := 0; i < 200; i++ {
		var c channelModeMachine
		c.begin(ModeLive, false, 0)
		c.PromoteStatic()

		var wins atomic.Int32
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			by := claimManual
			if g%2 == 0 {
				by = claimAuto
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ok := c.Claim(by); ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if n := wins.Load(); n != 1 {
			t.Fatalf("iteration %d: %d claims won, want exactly 1", i, n)
		}
	}
}

// The suppression a restore installs holds until the stored count passes the
// baseline, then lifts for good. A one-way flag would leave a restored channel
// unable to ever transition on its own again.
func TestChannelModeSuppressionLiftsPastBaseline(t *testing.T) {
	var c channelModeMachine
	c.begin(ModeLive, true, 5)

	if c.AutoAllowed(5) {
		t.Fatal("auto-VOD allowed with nothing stored past the baseline")
	}
	if !c.AutoAllowed(6) {
		t.Fatal("auto-VOD still suppressed after a new segment")
	}
	if !c.AutoAllowed(0) {
		t.Error("the suppression came back after it lifted")
	}
}

// Only a successful manual claim overrides the suppression: an automatic
// claim is exactly what it exists to hold back, and a refused claim changed
// nothing.
func TestChannelModeClaimAndSuppression(t *testing.T) {
	tests := []struct {
		name           string
		from           ChannelMode
		by             claimTrigger
		wantSuppressed bool
	}{
		{"manual claim lifts it", ModeLive, claimManual, false},
		{"auto claim keeps it", ModeStaticIngesting, claimAuto, true},
		{"refused manual claim keeps it", ModeVOD, claimManual, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c channelModeMachine
			c.begin(ModeLive, true, 1000)
			c.mode.Store(int32(tc.from))

			c.Claim(tc.by)

			if got := !c.AutoAllowed(0); got != tc.wantSuppressed {
				t.Errorf("suppressed = %v, want %v", got, tc.wantSuppressed)
			}
		})
	}
}
