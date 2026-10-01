package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"time"
)

func (ctx *AppContext) reconnectMQTT() {
	ctx.mqttMu.Lock()
	old := ctx.MQTTClient
	ctx.MQTTClient = nil
	ctx.mqttMu.Unlock()
	if old != nil {
		old.Disconnect(250)
	}
	c := initMQTT(ctx)
	ctx.mqttMu.Lock()
	ctx.MQTTClient = c
	ctx.mqttMu.Unlock()
}

// reloadVirtualKeyz2ConfigLive reads ConfigPath from disk, applies settings, updates log filter, tech prompt, and MQTT.
func reloadVirtualKeyz2ConfigLive(ctx *AppContext) error {
	path := strings.TrimSpace(ctx.ConfigPath)
	if path == "" {
		path = "virtualkeyz2.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw virtualkeyz2JSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}
	ctx.configMu.Lock()
	if err := applyVirtualKeyz2JSON(ctx, &raw); err != nil {
		ctx.configMu.Unlock()
		return err
	}
	lvl := ctx.Config.LogLevel
	prompt := ctx.TechMenuPrompt
	ctx.configMu.Unlock()
	registerTechMenuPrompt(prompt)
	syncLogFilterFromConfigLevel(lvl)
	log.Println("INFO: Configuration reloaded from disk (MQTT reconnecting; GPIO / relay_output_mode changes need a full restart).")
	lcdShowConfigReload(ctx)
	ctx.reconnectMQTT()
	ctx.techHistoryTrimToMax()
	ctx.syncFiremansServiceAfterConfigReload()
	ctx.syncFireAlarmAfterConfigReload()
	return nil
}

func effectiveConfigPath(ctx *AppContext) string {
	p := strings.TrimSpace(ctx.ConfigPath)
	if p == "" {
		return "virtualkeyz2.json"
	}
	return p
}

// applyInMemoryConfigLive reapplies current in-memory settings: log filter, tech prompt, MQTT reconnect.
func applyInMemoryConfigLive(ctx *AppContext) {
	ctx.configMu.RLock()
	prompt := ctx.TechMenuPrompt
	lvl := ctx.Config.LogLevel
	ctx.configMu.RUnlock()
	registerTechMenuPrompt(prompt)
	syncLogFilterFromConfigLevel(lvl)
	log.Println("INFO: In-memory configuration applied live (MQTT reconnect; GPIO pin map unchanged until reboot).")
	lcdShowConfigReload(ctx)
	ctx.reconnectMQTT()
	ctx.syncFiremansServiceAfterConfigReload()
	ctx.syncFireAlarmAfterConfigReload()
}

// restartCurrentProgram replaces this OS process with a new instance of the same executable, preserving
// os.Args[1:] and the environment. On success it does not return. Use after cfg reload when GPIO or other
// hardware must be reopened from a clean process (live reload does not re-run GPIO setup).
func restartCurrentProgram() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	argv := make([]string, 0, len(os.Args))
	argv = append(argv, exe)
	argv = append(argv, os.Args[1:]...)
	auditLogFlush(2 * time.Second)
	return syscall.Exec(exe, argv, os.Environ())
}
