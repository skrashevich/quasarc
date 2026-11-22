package station

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	alice "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice"
	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"github.com/skrashevich/quasarc/protocol"
	"google.golang.org/protobuf/proto"
)

const aliceCapability = "AliceCapability"

// Speak queues text in Alice's native voice and waits for playback to start
// and finish. The caller's deadline covers both synthesis and playback. When
// none is set, Speak waits up to 30 seconds.
func (c Client) Speak(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("empty speech text")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	port, layout := c.Port, protocol.Auto
	if port == 0 {
		path, ports, err := loadConfig(c.ConfigPath, readSpeechConfig)
		if err != nil {
			return err
		}
		port = cmp.Or(ports["aliced"], 9875)
		if path != "" {
			layout = DefaultFraming(BuildFromPorts(ports))
		}
	}
	conn, err := dialService(ctx, net.JoinHostPort(cmp.Or(c.Host, "127.0.0.1"), strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	bus := protocol.NewBus(conn, layout, protocol.Envelope)
	deadline, _ := ctx.Deadline()
	if err := bus.SetReadDeadline(deadline); err != nil {
		return err
	}
	err = vocalize(bus, text)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func vocalize(bus *protocol.Bus, text string) error {
	listen := &ipc.Remoting_CapabilityAddListenerMethod{CapabilityId: new(aliceCapability)}
	speak := &ipc.Remoting_AliceCapabilityMethod{
		Method: ipc.Remoting_AliceCapabilityMethod_START_VOCALIZE.Enum(),
		Events: new(true),
		VocalizeDirective: &alice.TAliceCapability_TVocalizeDirective{
			Text:          &text,
			SynthesisMode: alice.TAliceCapability_TVocalizeDirective_Append.Enum(),
		},
	}
	for _, call := range []*ipc.Remoting{
		{RemoteObjectId: new(aliceCapability), Method: &ipc.Remoting_CapabilityAddListenerMethod_{CapabilityAddListenerMethod: listen}},
		{RemoteObjectId: new(aliceCapability), Method: &ipc.Remoting_AliceCapabilityMethod_{AliceCapabilityMethod: speak}},
	} {
		payload, err := proto.Marshal(call)
		if err != nil {
			return err
		}
		if err := bus.WriteFrame(protocol.Frame{ID: uint64(ipc.QuasarMessage_TYPE_REMOTING), WT: 2, Payload: payload}); err != nil {
			return err
		}
	}
	var startedID *string
	for {
		body, err := bus.ReadMessageLimit(512 << 10)
		if err != nil {
			return err
		}
		var msg ipc.QuasarMessage
		if err := proto.Unmarshal(body, &msg); err != nil {
			return err
		}
		event := speechEvent(&msg)
		if event == nil {
			continue
		}
		id := event.GetRequestId()
		switch event.GetMethod() {
		case ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED:
			if startedID != nil && *startedID != id {
				return errors.New("overlapping Alice playback")
			}
			startedID = &id
		case ipc.Remoting_AliceCapabilityListenerMethod_TTS_COMPLETED:
			// Station 2 sets the ID on STARTED and omits it on COMPLETED.
			if startedID != nil && (id == "" || id == *startedID) {
				return nil
			}
		case ipc.Remoting_AliceCapabilityListenerMethod_ON_ALICE_ERROR:
			return errors.New("Alice speech failed")
		}
	}
}

func speechEvent(msg *ipc.QuasarMessage) *ipc.Remoting_AliceCapabilityListenerMethod {
	call := msg.GetRemoting()
	if call == nil {
		// Some firmware sends AliceCapability callbacks in AliceState envelopes.
		if msg.GetType() != ipc.QuasarMessage_TYPE_REMOTING && msg.GetType() != ipc.QuasarMessage_TYPE_ALICE_STATE {
			return nil
		}
		call = new(ipc.Remoting)
		if proto.Unmarshal(msg.GetPayload(), call) != nil {
			return nil
		}
	}
	if name := call.GetRemoteObjectId(); name != "" && name != aliceCapability {
		return nil
	}
	return call.GetAliceCapabilityListenerMethod()
}
