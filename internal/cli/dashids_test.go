package cli

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

const (
	dashedID    = "-bJxDLEMvt-Z6t4Yna7V8SYQ_FIHWT2_QbBr-whe-bIE8rbZunzr5RhXGaihvQ43z2qcxcqFgVRwi7A=="
	plainID     = "NWM5AYGxFIHWT2_QbBr-whe-bIE8rbZunzr5RhXGaihvQ43z2qcxcqFgVRwi7A5C-ADmohv7TjXfYbDEIHZPQ=="
	shortDashed = "-abc=="
)

func TestLooksLikeDashedProtonID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical leading-dash ID", dashedID, true},
		{"plain ID without leading dash", plainID, false},
		{"verbose long flag", "--verbose", false},
		{"short flag", "-v", false},
		{"too short", shortDashed, false},
		{"missing == suffix", "-bJxDLEMvt-Z6t4Yna7V8SYQ_FIHWT2_QbBr-whe-bIE8rbZunzr5RhXGaihvQ43z2qcxcqFgVRwi7A", false},
		{"non-base64 chars", "-bJxDLEMvt-Z6t4Yna7V8SYQ_FIHWT2_QbBr!whe$bIE8rbZunzr5RhXGaihvQ43z2qcxcqFgVRwi7A==", false},
		{"empty", "", false},
		{"single dash", "-", false},
		{"flag with eq sign", "--name=John Doe Long Title Goes Here Lorem ipsum aaaaaaaa==", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeDashedProtonID(tc.in); got != tc.want {
				t.Errorf("looksLikeDashedProtonID(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPreprocessArgs(t *testing.T) {
	t.Run("plain ID untouched", func(t *testing.T) {
		in := []string{"proton-cli", "mail", "messages", "read", plainID}
		if got := preprocessArgs(in); len(got) != len(in) {
			t.Fatalf("preprocessArgs grew args: %v -> %v", in, got)
		}
	})
	t.Run("dashed ID gets -- inserted", func(t *testing.T) {
		in := []string{"proton-cli", "mail", "messages", "read", dashedID}
		want := []string{"proton-cli", "mail", "messages", "read", "--", dashedID}
		if got := preprocessArgs(in); !equalSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("dashed ID after flag value", func(t *testing.T) {
		in := []string{"proton-cli", "mail", "messages", "read", "--format", "raw", dashedID}
		want := []string{"proton-cli", "mail", "messages", "read", "--format", "raw", "--", dashedID}
		if got := preprocessArgs(in); !equalSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("only first dashed ID gets --, rest protected by terminator", func(t *testing.T) {
		in := []string{"proton-cli", "mail", "messages", "trash", dashedID, dashedID}
		want := []string{"proton-cli", "mail", "messages", "trash", "--", dashedID, dashedID}
		if got := preprocessArgs(in); !equalSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("user already inserted -- leaves args alone", func(t *testing.T) {
		in := []string{"proton-cli", "mail", "messages", "read", "--", dashedID}
		if got := preprocessArgs(in); !equalSlice(got, in) {
			t.Errorf("preprocessArgs altered args after user --: got %v, want %v", got, in)
		}
	})
	t.Run("real flag never matches", func(t *testing.T) {
		in := []string{"proton-cli", "--verbose", "mail", "messages", "list"}
		if got := preprocessArgs(in); !equalSlice(got, in) {
			t.Errorf("preprocessArgs altered args with real flag: got %v, want %v", got, in)
		}
	})
	// A dashed id used as a FLAG VALUE must stay glued to its flag: injecting the
	// terminator between them would hand --calendar the value "--" and strand the id.
	t.Run("dashed ID as a long-flag value is left glued to its flag", func(t *testing.T) {
		in := []string{"proton-cli", "calendar", "events", "create", "--calendar", dashedID, "--title", "x"}
		if got := preprocessArgs(in); !equalSlice(got, in) {
			t.Errorf("preprocessArgs split a flag from its value: got %v, want %v", got, in)
		}
	})
	t.Run("dashed ID as a short-flag value is left glued to its flag", func(t *testing.T) {
		in := []string{"proton-cli", "calendar", "events", "create", "-c", dashedID}
		if got := preprocessArgs(in); !equalSlice(got, in) {
			t.Errorf("preprocessArgs split a short flag from its value: got %v, want %v", got, in)
		}
	})
	// --flag=value is self-contained, so a positional dashed id right after it is
	// still a positional and must be protected.
	t.Run("dashed ID after an =-form flag is still protected", func(t *testing.T) {
		in := []string{"proton-cli", "calendar", "events", "delete", "--calendar=" + plainID, dashedID}
		want := []string{"proton-cli", "calendar", "events", "delete", "--calendar=" + plainID, "--", dashedID}
		if got := preprocessArgs(in); !equalSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	// The shape our embed client now emits: flags first, explicit terminator, ids last.
	t.Run("client shape with explicit -- is left untouched", func(t *testing.T) {
		in := []string{"proton-cli", "calendar", "events", "update", "--title", "x", "--", plainID, dashedID}
		if got := preprocessArgs(in); !equalSlice(got, in) {
			t.Errorf("preprocessArgs altered an explicitly terminated argv: got %v, want %v", got, in)
		}
	})
}

func TestIsFlagToken(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"--calendar", true},
		{"-c", true},
		{"--calendar=abc", false},
		{"--", false},
		{"-", false},
		{"", false},
		{"list", false},
		{dashedID, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := isFlagToken(tc.in); got != tc.want {
				t.Errorf("isFlagToken(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewrapFlagError(t *testing.T) {
	t.Run("non-pflag error passes through", func(t *testing.T) {
		err := errors.New("some other error")
		if got := rewrapFlagError(err, []string{"proton-cli"}); got != err {
			t.Errorf("rewrapFlagError altered non-pflag error: got %v", got)
		}
	})
	t.Run("nil passes through", func(t *testing.T) {
		if rewrapFlagError(nil, []string{"proton-cli"}) != nil {
			t.Error("rewrapFlagError(nil) != nil")
		}
	})
	t.Run("pflag shorthand error with ID-shape token rewraps", func(t *testing.T) {
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		err := fs.Parse([]string{dashedID})
		if err == nil {
			t.Fatal("expected pflag parse error")
		}
		gotMsg := rewrapFlagError(err, []string{"proton-cli"}).Error()
		if !strings.Contains(gotMsg, "looks like a flag") || !strings.Contains(gotMsg, "insert -- before it") || !strings.Contains(gotMsg, dashedID) {
			t.Errorf("unexpected rewrapped message: %s", gotMsg)
		}
	})
	t.Run("pflag shorthand error with non-ID token passes through", func(t *testing.T) {
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		err := fs.Parse([]string{"-xyz"})
		if err == nil {
			t.Fatal("expected pflag parse error")
		}
		if strings.Contains(rewrapFlagError(err, []string{"proton-cli"}).Error(), "looks like a flag because") {
			t.Error("rewrap fired on non-ID token")
		}
	})
	t.Run("cobra accepts-N-args error with dashed ID rewraps", func(t *testing.T) {
		err := errors.New("accepts 1 arg(s), received 3")
		argv := []string{"proton-cli", "mail", "messages", "read", dashedID, "--format", "raw"}
		gotMsg := rewrapFlagError(err, argv).Error()
		if !strings.Contains(gotMsg, "insert -- before it") || !strings.Contains(gotMsg, "Put flags") || !strings.Contains(gotMsg, dashedID) {
			t.Errorf("unexpected message: %s", gotMsg)
		}
	})
	t.Run("cobra accepts-N-args error without dashed ID passes through", func(t *testing.T) {
		err := errors.New("accepts 1 arg(s), received 3")
		argv := []string{"proton-cli", "mail", "messages", "read", plainID, "--format", "raw"}
		if strings.Contains(rewrapFlagError(err, argv).Error(), "insert -- before it") {
			t.Error("rewrap fired without dashed ID")
		}
	})
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
