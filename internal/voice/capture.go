// Package voice replicates Claude Code's voice-input stack (mod_1739
// capture, mod_1710 stream, mod_1711 controller, mod_0747 UX contract).
package voice

import (
	"context"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Capture format constants — real uses 16kHz mono signed 16-bit PCM (k, x
// in mod_1739).
const (
	SampleRate = 16000
	Channels   = 1
)

// Message texts are verbatim from mod_1739 ue()/ce().
const (
	msgRemoteNoDevice = `Voice mode requires microphone access, but no audio device is available in this environment.

To use voice mode, run Claude Code locally instead.`

	msgWSL = `Voice mode could not find a working audio recorder in WSL.

WSL2 with WSLg provides audio via PulseAudio — install SoX with its PulseAudio backend (sudo apt install sox libsox-fmt-pulse) so Claude Code can record through it.

If WSLg is not available (for example WSL1), run Claude Code in native Windows instead.`

	msgNoMic = `Voice mode requires a microphone, but SoX could not open an audio capture device.

This usually means the host has no microphone (for example, a remote server). Run Claude Code on a machine with a microphone to use voice input.`

	msgSoxManual = `Voice mode requires SoX for audio recording. Install SoX manually:
  macOS: brew install sox
  Ubuntu/Debian: sudo apt-get install sox
  Fedora: sudo dnf install sox`
)

// Availability is the result of CheckRecordingAvailability — mirrors the
// {available, reason} shape the real module returns.
type Availability struct {
	Available bool
	Reason    string
}

// DependencyReport is the result of CheckVoiceDependencies.
type DependencyReport struct {
	Available      bool
	Missing        []string
	InstallCommand string
}

// seams for tests — real probes `<cmd> --version` with a 3s timeout (u()).
var (
	probeVersion = func(name string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, name, "--version").Run() == nil
	}
	isSSH = func() bool {
		return os.Getenv("SSH_CONNECTION") != "" ||
			os.Getenv("SSH_CLIENT") != "" ||
			os.Getenv("SSH_TTY") != ""
	}
	isWSL = func() bool {
		if os.Getenv("WSL_DISTRO_NAME") != "" {
			return true
		}
		if runtime.GOOS != "linux" {
			return false
		}
		b, err := os.ReadFile("/proc/version")
		return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
	}
	goos = runtime.GOOS
)

// CheckRecordingAvailability mirrors ue() in mod_1739: remote-env guard,
// WSL messaging, sox/rec probes, brew install hint, no-mic fallback.
// (The NAPI native-capture path doesn't exist in Go — skipped like a
// platform where isNativeAudioAvailable() is false.)
func CheckRecordingAvailability() Availability {
	if isSSH() || os.Getenv("CLAUDE_CODE_REMOTE") != "" {
		return Availability{Available: false, Reason: msgRemoteNoDevice}
	}
	sox := probeVersion("sox")
	if sox && probeVersion("rec") {
		return Availability{Available: true}
	}
	if isWSL() {
		return Availability{Available: false, Reason: msgWSL}
	}
	if !sox {
		if probeVersion("brew") {
			return Availability{Available: false,
				Reason: "Voice mode requires SoX for audio recording. Install it with: brew install sox"}
		}
		return Availability{Available: false, Reason: msgSoxManual}
	}
	return Availability{Available: false, Reason: msgNoMic}
}

// CheckVoiceDependencies mirrors ce() in mod_1739 — used by /voice before
// enabling.
func CheckVoiceDependencies() DependencyReport {
	var missing []string
	if !probeVersion("sox") {
		missing = append(missing, "sox (rec command)")
	}
	install := ""
	if len(missing) > 0 && probeVersion("brew") {
		install = "brew install sox"
	}
	return DependencyReport{
		Available:      len(missing) == 0,
		Missing:        missing,
		InstallCommand: install,
	}
}

// CaptureStart mirrors the {started, superseded, hint, expected} result
// the real startRecording returns.
type CaptureStart struct {
	Started bool
	Hint    string // set when a better error than the default exists
}

// Recorder is a live `rec` (SoX) subprocess streaming s16le PCM on stdout.
type Recorder struct {
	cmd *exec.Cmd
}

// StartRecorder spawns `rec -q --buffer 1024 -t raw -r 16000 -e signed
// -b 16 -c 1 -` (+ SoX silence filter when silenceDetection is on, args
// "silence 1 0.1 3% 1 2.0 3%" — E/S constants in mod_1739). Chunks go to
// onChunk; process exit (any reason) calls onEnd once.
//
// The real session recorder passes {silenceDetection:false}; the flag is
// kept for parity with the permission-probe recorder.
func StartRecorder(onChunk func([]byte), onEnd func(), silenceDetection bool) (*Recorder, CaptureStart) {
	args := []string{"-q", "--buffer", "1024", "-t", "raw",
		"-r", "16000", "-e", "signed", "-b", "16", "-c", "1", "-"}
	if silenceDetection {
		args = append(args, "silence", "1", "0.1", "3%", "1", "2.0", "3%")
	}
	cmd := exec.Command("rec", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, CaptureStart{Started: false}
	}
	cmd.Stderr = nil // real pipes stderr and ignores it
	if err := cmd.Start(); err != nil {
		return nil, CaptureStart{Started: false}
	}
	r := &Recorder{cmd: cmd}
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				onChunk(chunk)
			}
			if err != nil {
				break
			}
		}
	}()
	go func() {
		cmd.Wait()
		onEnd()
	}()
	return r, CaptureStart{Started: true}
}

// Stop kills the recorder with SIGTERM like real's stopRecording.
func (r *Recorder) Stop() {
	if r == nil || r.cmd == nil || r.cmd.Process == nil {
		return
	}
	r.cmd.Process.Signal(sigTerm)
	r.cmd.Process.Kill()
}

// AudioLevel is the RMS-based signal level J() in mod_1711: RMS of s16le
// samples, normalized to [0,1] via min(rms/2000,1) then sqrt.
func AudioLevel(pcm []byte) float64 {
	n := len(pcm) >> 1
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(pcm[i]) | int16(pcm[i+1])<<8
		sum += float64(s) * float64(s)
	}
	rms := math.Sqrt(sum / float64(n))
	l := rms / 2000
	if l > 1 {
		l = 1
	}
	return math.Sqrt(l)
}
