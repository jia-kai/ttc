package graphics

import (
	"bytes"
	"testing"
)

func TestProbeReplyIdentityFragmentationAndInputPreservation(t *testing.T) {
	for _, test := range []struct {
		input            string
		supported, found bool
		pending          string
	}{
		{"typed\x1b_Gi=31337;OK\x1b\\more", true, true, "typedmore"},
		{"\x1b_Gi=99;OK\x1b\\\x1b_Gi=31337;OK\x1b\\", true, true, "\x1b_Gi=99;OK\x1b\\"},
		{"\x1b_Gi=313370;OK\x1b\\", false, false, "\x1b_Gi=313370;OK\x1b\\"},
		{"\x1b_Gi=31337;ENOTSUP\x1b\\", false, true, ""},
		{"\x1b_Gi=31337;OK", false, false, "\x1b_Gi=31337;OK"},
		{"\x1b[Ax", false, false, "\x1b[Ax"},
	} {
		supported, found, pending := probeReply([]byte(test.input))
		if supported != test.supported || found != test.found || string(pending) != test.pending {
			t.Fatal(test, supported, found, string(pending))
		}
	}
	complete := []byte("\x1b_Gi=31337;OK\x1b\\")
	for i := 0; i < len(complete); i++ {
		_, found, pending := probeReply(complete[:i])
		if found || !bytes.Equal(pending, complete[:i]) {
			t.Fatal("consumed incomplete reply", i)
		}
	}
}
