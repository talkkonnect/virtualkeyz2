package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"virtualkeyz2/internal/remotemqtt"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// mqttInitialConnectTimeout bounds the first broker connection attempt; if the broker is unreachable,
// initMQTT returns nil so the rest of the process starts normally.
const mqttInitialConnectTimeout = 5 * time.Second

func initMQTT(ctx *AppContext) mqtt.Client {
	ctx.configMu.RLock()
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	if !cfg.MQTTEnabled {
		log.Println("INFO: MQTT disabled (MQTTEnabled false).")
		return nil
	}
	if strings.TrimSpace(cfg.MQTTBroker) == "" {
		log.Println("INFO: MQTT disabled (MQTTBroker empty).")
		return nil
	}

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.MQTTBroker).
		SetClientID(cfg.MQTTClientID).
		SetConnectTimeout(mqttInitialConnectTimeout).
		SetConnectRetry(false).
		SetAutoReconnect(true)

	if cfg.MQTTUsername != "" {
		opts.SetUsername(cfg.MQTTUsername)
		opts.SetPassword(cfg.MQTTPassword)
	}

	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		ctx.configMu.RLock()
		en := ctx.Config.MQTTEnabled
		br := strings.TrimSpace(ctx.Config.MQTTBroker)
		ctx.configMu.RUnlock()
		if en && br != "" {
			log.Printf("WARNING: MQTT connection lost: %v", err)
			lcdShowMQTTOffline(ctx)
		}
	})
	opts.SetOnConnectHandler(func(c mqtt.Client) {
		ctx.configMu.RLock()
		topic := strings.TrimSpace(ctx.Config.MQTTCommandTopic)
		pairTopic := strings.TrimSpace(ctx.Config.MQTTPairPeerTopic)
		mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
		role := normalizePairPeerRole(ctx.Config.PairPeerRole)
		mqEn := ctx.Config.MQTTEnabled
		br := strings.TrimSpace(ctx.Config.MQTTBroker)
		ctx.configMu.RUnlock()
		if mqEn && br != "" {
			lcdShowMQTTRecovered(ctx)
		}
		if topic != "" {
			h := mqttRemoteMessageHandler(ctx)
			if t := c.Subscribe(topic, 1, h); t.Wait() && t.Error() != nil {
				log.Printf("WARNING: MQTT subscribe %q: %v", topic, t.Error())
			} else {
				log.Printf("INFO: MQTT remote control subscribed to %q", topic)
			}
		}
		if pairTopic != "" && pairedExitSubscribesToPeer(mode, role) {
			ph := mqttPairPeerMessageHandler(ctx)
			if t := c.Subscribe(pairTopic, 1, ph); t.Wait() && t.Error() != nil {
				log.Printf("WARNING: MQTT pair-peer subscribe %q: %v", pairTopic, t.Error())
			} else {
				log.Printf("INFO: MQTT pair-peer (exit station) subscribed to %q", pairTopic)
			}
		}
	})

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(mqttInitialConnectTimeout) {
		log.Printf("WARNING: MQTT broker %q not reachable within %v; MQTT disabled for this run.", cfg.MQTTBroker, mqttInitialConnectTimeout)
		client.Disconnect(250)
		return nil
	}
	if err := token.Error(); err != nil {
		log.Printf("WARNING: MQTT connection failed: %v; MQTT disabled for this run.", err)
		client.Disconnect(250)
		return nil
	}
	log.Printf("INFO: MQTT connected to %q", cfg.MQTTBroker)
	return client
}

var mqttRemoteMu sync.Mutex

func mqttRemoteMessageHandler(ctx *AppContext) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		handleMQTTRemotePayload(ctx, m.Topic(), m.Payload())
	}
}

func handleMQTTRemotePayload(ctx *AppContext, topic string, payload []byte) {
	mqttRemoteMu.Lock()
	defer mqttRemoteMu.Unlock()

	ctx.mqttMu.RLock()
	clientOK := ctx.MQTTClient != nil && ctx.MQTTClient.IsConnected()
	ctx.mqttMu.RUnlock()
	if !clientOK {
		log.Printf("WARNING: MQTT remote command ignored (client not connected). topic=%s", topic)
		return
	}

	p := bytes.TrimSpace(payload)
	var req remotemqtt.RemoteCommand
	jsonOK := json.Unmarshal(p, &req) == nil && strings.TrimSpace(req.Cmd) != ""
	cmd := ""
	if jsonOK {
		cmd = strings.TrimSpace(req.Cmd)
	} else {
		cmd = strings.TrimSpace(string(p))
	}
	cmdLower := strings.ToLower(cmd)

	ctx.configMu.RLock()
	token := ctx.Config.MQTTCommandToken
	cfg := ctx.Config
	ctx.configMu.RUnlock()

	if token != "" {
		if !jsonOK || req.Token != token {
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "invalid or missing token (JSON + token required)"})
			log.Println("WARNING: MQTT remote command rejected (bad token or non-JSON payload).")
			return
		}
	}

	if cmdLower == "" {
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Error: "empty_command"})
		return
	}

	switch cmdLower {
	case "open_door", "door_open", "unlock":
		log.Printf("INFO: MQTT remote: open door (topic=%s)", topic)
		if ctx.FiremansServiceActive() {
			debugf("Fireman's service: MQTT door open ignored (all access relays held off).")
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "firemans_service_active", Detail: "door relay not pulsed during emergency bypass"})
			fireEventWebhook(ctx, "mqtt_remote_door_open_denied", map[string]any{"mqtt_topic": topic, "reason": "firemans_service_active"})
			return
		}
		if ctx.GPIO != nil {
			if err := ctx.GPIO.ActionPulseChecked("door", cfg.RelayPulseDuration, ctx.doorPulseOffErrReporter(map[string]any{"mqtt_topic": topic, "source": "mqtt_remote_door_open"})); err != nil {
				mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "hardware_actuation_failed", Detail: err.Error()})
				fireEventWebhook(ctx, "hardware_actuation_failed", map[string]any{"mqtt_topic": topic, "source": "mqtt_remote_door_open", "error": err.Error()})
				playSoundEnabled(cfg, cfg.SoundPinReject, cfg.SoundPinRejectEnabled, cfg.SoundPinRejectBlocking)
				log.Printf("ERROR: MQTT open_door: door relay actuation failed: %v", err)
				return
			}
			ctx.pulseAuthorizedAccessAuxRelays(cfg)
			playSoundEnabled(cfg, cfg.SoundPinOK, cfg.SoundPinOKEnabled, cfg.SoundPinOKBlocking)
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, Detail: "door relay pulsed"})
			fireEventWebhook(ctx, "mqtt_remote_door_open", map[string]any{"mqtt_topic": topic})
		} else {
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "gpio_unavailable"})
			log.Println("WARNING: MQTT open_door: GPIO unavailable.")
		}

	case "firemans_service_on", "firemans_on", "emergency_bypass_on":
		if !cfg.FiremansServiceEnabled {
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "firemans_service_disabled_in_config"})
			return
		}
		log.Printf("INFO: MQTT remote: fireman's service ON (topic=%s)", topic)
		ctx.applyFiremansServiceTransition(true, "mqtt:"+cmdLower)
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, Detail: "firemans_service_activated"})
	case "firemans_service_off", "firemans_off", "emergency_bypass_off":
		if !cfg.FiremansServiceEnabled {
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "firemans_service_disabled_in_config"})
			return
		}
		log.Printf("INFO: MQTT remote: fireman's service OFF (topic=%s)", topic)
		ctx.applyFiremansServiceTransition(false, "mqtt:"+cmdLower)
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, Detail: "firemans_service_deactivated"})
	case "firemans_service_status", "firemans_status":
		active := ctx.FiremansServiceActive()
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, Detail: fmt.Sprintf("firemans_service_active=%v", active)})

	case "buzzer", "buzz", "alarm":
		log.Printf("INFO: MQTT remote: buzzer (topic=%s)", topic)
		if ctx.FiremansServiceActive() {
			debugf("Fireman's service: MQTT buzzer ignored (buzzer relay held off).")
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "firemans_service_active"})
			return
		}
		if ctx.GPIO != nil {
			ctx.GPIO.ActionPulse("buzzer", cfg.BuzzerRelayPulseDuration)
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, Detail: "buzzer relay pulsed"})
			fireEventWebhook(ctx, "mqtt_remote_buzzer", map[string]any{"mqtt_topic": topic})
		} else {
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "gpio_unavailable"})
		}

	case "door_status", "status_door":
		if ctx.GPIO == nil || !ctx.GPIO.DoorSensorConfigured() {
			mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "door_sensor_unavailable"})
			return
		}
		open := ctx.GPIO.DoorIsOpen(cfg.DoorSensorClosedIsLow)
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, DoorOpen: &open})

	case "ping", "hello":
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: true, Cmd: cmdLower, Detail: "pong"})

	default:
		mqttPublishRemoteAck(ctx, remotemqtt.RemoteAck{OK: false, Cmd: cmdLower, Error: "unknown_command"})
		log.Printf("WARNING: MQTT remote unknown cmd %q", cmd)
	}
}

// mqttPublishWaitTimeout bounds how long a QoS1 publish waits for the broker ack, so a dropped
// link cannot stall the caller (the keypad grant path publishes pair-peer hints).
const mqttPublishWaitTimeout = 2 * time.Second

func mqttPublishRemoteAck(ctx *AppContext, ack remotemqtt.RemoteAck) {
	ctx.mqttMu.RLock()
	client := ctx.MQTTClient
	ctx.mqttMu.RUnlock()
	if client == nil || !client.IsConnected() {
		return
	}
	ctx.configMu.RLock()
	topic := strings.TrimSpace(ctx.Config.MQTTStatusTopic)
	ctx.configMu.RUnlock()
	if topic == "" {
		return
	}
	b, err := json.Marshal(ack)
	if err != nil {
		return
	}
	if t := client.Publish(topic, 1, false, b); !t.WaitTimeout(mqttPublishWaitTimeout) {
		log.Printf("WARNING: MQTT publish status: no broker ack within %s", mqttPublishWaitTimeout)
	} else if t.Error() != nil {
		log.Printf("WARNING: MQTT publish status: %v", t.Error())
	}
}

var mqttPairPeerMu sync.Mutex

func mqttPairPeerMessageHandler(ctx *AppContext) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		handleMQTTPairPeerPayload(ctx, m.Payload())
	}
}

func handleMQTTPairPeerPayload(ctx *AppContext, payload []byte) {
	mqttPairPeerMu.Lock()
	defer mqttPairPeerMu.Unlock()
	ctx.configMu.RLock()
	tokExpect := strings.TrimSpace(ctx.Config.PairPeerToken)
	mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
	role := normalizePairPeerRole(ctx.Config.PairPeerRole)
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	if !pairedExitSubscribesToPeer(mode, role) {
		return
	}
	var msg remotemqtt.PairPeerMessage
	if json.Unmarshal(bytes.TrimSpace(payload), &msg) != nil || strings.TrimSpace(msg.Cmd) == "" {
		log.Println("WARNING: pair-peer MQTT: invalid JSON payload")
		return
	}
	if tokExpect != "" && msg.Token != tokExpect {
		log.Println("WARNING: pair-peer MQTT: rejected (bad token)")
		return
	}
	cmd := strings.ToLower(strings.TrimSpace(msg.Cmd))
	if cmd != "pulse_paired_exit" && cmd != "unlock_peer_exit" {
		return
	}
	log.Println("INFO: pair-peer MQTT: entry station requested coordinated exit unlock; pulsing local door relay.")
	fireEventWebhook(ctx, "mqtt_pair_peer_exit_pulse", map[string]any{"cmd": cmd, "operation_mode": mode})
	if ctx.GPIO != nil {
		ctx.GPIO.ActionPulse("door", cfg.RelayPulseDuration)
		ctx.pulseAuthorizedAccessAuxRelays(cfg)
	}
}

func publishMQTTPairPeerPulse(ctx *AppContext, cfg DeviceConfig) {
	ctx.mqttMu.RLock()
	client := ctx.MQTTClient
	ctx.mqttMu.RUnlock()
	if client == nil || !client.IsConnected() {
		log.Println("WARNING: pair-peer MQTT publish skipped (client not connected)")
		return
	}
	topic := strings.TrimSpace(cfg.MQTTPairPeerTopic)
	if topic == "" {
		return
	}
	body, err := json.Marshal(remotemqtt.PairPeerMessage{Cmd: "pulse_paired_exit", Token: cfg.PairPeerToken})
	if err != nil {
		return
	}
	t := client.Publish(topic, 1, false, body)
	if !t.WaitTimeout(mqttPublishWaitTimeout) {
		log.Printf("WARNING: pair-peer MQTT publish: no broker ack within %s", mqttPublishWaitTimeout)
		return
	}
	if err := t.Error(); err != nil {
		log.Printf("WARNING: pair-peer MQTT publish: %v", err)
		return
	}
	log.Printf("INFO: pair-peer MQTT: published exit-unlock hint to %q", topic)
}
