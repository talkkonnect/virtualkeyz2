// Package remotemqtt holds JSON payloads for MQTT remote control and pair-peer messaging.
package remotemqtt

// RemoteCommand is the expected JSON body on the MQTT command topic.
type RemoteCommand struct {
	Cmd   string `json:"cmd"`
	Token string `json:"token,omitempty"`
}

// RemoteAck is published to the MQTT status topic when configured.
type RemoteAck struct {
	OK     bool   `json:"ok"`
	Cmd    string `json:"cmd"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	// DoorOpen is set for cmd door_status when GPIO is available.
	DoorOpen *bool `json:"door_open,omitempty"`
}

// PairPeerMessage is the JSON body for paired entry/exit controllers.
type PairPeerMessage struct {
	Cmd   string `json:"cmd"`
	Token string `json:"token,omitempty"`
}
