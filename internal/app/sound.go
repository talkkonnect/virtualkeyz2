package app

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Sound is serialized per ALSA -D device so concurrent aplay (e.g. door alarm + keypad feedback) does not
// hit "Device or resource busy". Transient failures are retried with backoff; fatal ALSA/mime errors are not.
const (
	soundPerDeviceQueue = 64
	soundAplayMaxTries  = 60
	soundRetryBaseDelay = 100 * time.Millisecond
	soundRetryMaxDelay  = 2 * time.Second
)

type soundQueueJob struct {
	cfg  DeviceConfig
	path string
	// If non-nil, the worker sends nil or a terminal error (best-effort) when playback ends.
	done chan error
}

var (
	soundQueueMu  sync.Mutex
	soundQueueChs = make(map[string]chan *soundQueueJob) // key = SoundCardName ("" = default aplay device)
)

func alsaDeviceKeyForSound(cfg DeviceConfig) string {
	return cfg.SoundCardName
}

func aplayErrorProbablyFatal(hint string) bool {
	s := strings.ToLower(hint)
	if strings.Contains(s, "unknown pcm") {
		return true
	}
	if strings.Contains(s, "no soundcards found") {
		return true
	}
	if strings.Contains(s, "file format") && strings.Contains(s, "not recognized") {
		return true
	}
	return false
}

func aplayOneAttempt(cfg DeviceConfig, path string) (outStr string, err error) {
	args := []string{"-q"}
	if cfg.SoundCardName != "" {
		args = append(args, "-D", cfg.SoundCardName)
	}
	args = append(args, path)
	cmd := exec.Command("aplay", args...)
	out, e := cmd.CombinedOutput()
	return string(out), e
}

func aplayWithRetriesToDevice(cfg DeviceConfig, path string) error {
	var lastErr error
	delay := soundRetryBaseDelay
	for attempt := 1; attempt <= soundAplayMaxTries; attempt++ {
		out, err := aplayOneAttempt(cfg, path)
		if err == nil {
			return nil
		}
		lastErr = err
		hint := out + " " + err.Error()
		if aplayErrorProbablyFatal(hint) {
			break
		}
		if attempt < soundAplayMaxTries {
			if attempt == 1 {
				dev := cfg.SoundCardName
				if dev == "" {
					dev = "default"
				}
				debugf("aplay output not ready (device %q), will retry for %s: %v", dev, path, err)
			}
			time.Sleep(delay)
			nb := time.Duration(float64(delay) * 1.2)
			if nb > soundRetryMaxDelay {
				nb = soundRetryMaxDelay
			}
			delay = nb
		}
	}
	if lastErr != nil {
		log.Printf("WARNING: aplay failed for %s: %v", path, lastErr)
	}
	return lastErr
}

func soundQueueWorker(_ string, jobs <-chan *soundQueueJob) {
	for j := range jobs {
		if j == nil {
			continue
		}
		err := aplayWithRetriesToDevice(j.cfg, j.path)
		if j.done != nil {
			j.done <- err
		}
	}
}

func getSoundQueueCh(cfg DeviceConfig) chan *soundQueueJob {
	k := alsaDeviceKeyForSound(cfg)
	soundQueueMu.Lock()
	defer soundQueueMu.Unlock()
	ch, ok := soundQueueChs[k]
	if !ok {
		ch = make(chan *soundQueueJob, soundPerDeviceQueue)
		soundQueueChs[k] = ch
		go soundQueueWorker(k, ch)
	}
	return ch
}

// playSoundEnabled plays path when enabled is true, blocking until the sound
// finishes when blocking is true (sync) or returning immediately when false
// (async). This is the config-driven entry point: each sound's
// sound_<name>_blocking setting selects its behaviour.
func playSoundEnabled(cfg DeviceConfig, path string, enabled, blocking bool) {
	if !enabled {
		return
	}
	if blocking {
		playSoundSync(cfg, path)
		return
	}
	playSoundAsync(cfg, path)
}

// playSoundSync plays a WAV via ALSA aplay; blocks until finished (after queuing to the per-device player).
func playSoundSync(cfg DeviceConfig, path string) {
	if done := playSoundQueued(cfg, path); done != nil {
		<-done
	}
}

// playSoundQueued enqueues path on its device's player and returns a channel that receives once
// playback ends, or nil when there is nothing to play.
func playSoundQueued(cfg DeviceConfig, path string) <-chan error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		debugf("sound skipped (not found): %s", path)
		return nil
	}
	done := make(chan error, 1)
	getSoundQueueCh(cfg) <- &soundQueueJob{cfg: cfg, path: path, done: done}
	return done
}

func playSoundAsync(cfg DeviceConfig, path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		debugf("sound skipped (not found): %s", path)
		return
	}
	// One goroutine: enqueue; worker serializes aplay and retries if the device is busy.
	getSoundQueueCh(cfg) <- &soundQueueJob{cfg: cfg, path: path, done: nil}
}
