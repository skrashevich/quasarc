package station

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"github.com/skrashevich/quasarc/protocol"
)

func TestTelemetryStatusShapes(t *testing.T) {
	// An empty embedded message and an explicitly present zero are distinct.
	sdk := func(data []byte) protocol.Frame {
		return protocol.Frame{ID: 85, WT: 2, Payload: protocol.Frame{ID: 34, WT: 2, Payload: data}.EncodeDirect()}
	}
	direct := func(id uint64, data []byte) protocol.Frame { return protocol.Frame{ID: id, WT: 2, Payload: data} }
	emptyUpdate := `{"state":null,"code":null,"is_critical":null,"download_progress":null,"target_version":null}`
	activeUpdate := []byte{8, 2, 16, 1, 25}
	activeUpdate = binary.LittleEndian.AppendUint64(activeUpdate, math.Float64bits(0))
	activeUpdateJSON := `{"state":"downloading","code":2,"is_critical":true,"download_progress":0,"target_version":null}`
	for _, tc := range []struct {
		name  string
		fn    func(protocol.Frame) (any, error)
		input protocol.Frame
		want  string
	}{
		{"unrelated metric", metricaUUID, direct(24, []byte{10, 1, 'x'}), "null"},
		{"Linux UUID only", metricaUUID, direct(24, []byte{50, 1, 'a'}), `{"uuid":"a","metrica_device_id":null}`},
		{"Max distinct IDs", metricaUUID, direct(24, []byte{50, 1, 'a', 58, 1, 'b'}), `{"uuid":"a","metrica_device_id":"b"}`},
		{"unrelated IO event", sdkUpdateStatus, direct(85, []byte{8, 1}), "null"},
		{"empty SDK update", sdkUpdateStatus, sdk([]byte{58, 0}), emptyUpdate},
		{"SDK without update", sdkUpdateStatus, sdk([]byte{96, 0}), emptyUpdate},
		{"direct update with zero progress", updateStatus, direct(104, activeUpdate), activeUpdateJSON},
		{"same update nested", sdkUpdateStatus, sdk(protocol.Frame{ID: 7, WT: 2, Payload: activeUpdate}.EncodeDirect()), activeUpdateJSON},
		{"future state and target", updateStatus, direct(104, []byte{8, 9, 16, 0, 34, 2, 'v', '2'}), `{"state":"unknown","code":9,"is_critical":false,"download_progress":null,"target_version":"v2"}`},
		{"Linux no notifications", sdkNotificationStatus, sdk([]byte{96, 0}), `{"has_notifications":false,"count":null,"mode":"none"}`},
		{"Max no notifications", notificationStatus, direct(134, []byte{8, 0}), `{"has_notifications":false,"count":0,"mode":null}`},
		{"Max positive count", notificationStatus, direct(134, []byte{8, 3, 16, 1}), `{"has_notifications":true,"count":3,"mode":"available"}`},
		{"Linux passive", sdkNotificationStatus, sdk([]byte{96, 2}), `{"has_notifications":true,"count":null,"mode":"passive"}`},
		{"Linux unknown mode", sdkNotificationStatus, sdk([]byte{96, 9}), `{"has_notifications":null,"count":null,"mode":"unknown"}`},
		{"Linux missing mode", sdkNotificationStatus, sdk(nil), `{"has_notifications":null,"count":null,"mode":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			j, err := json.Marshal(got)
			if err != nil || string(j) != tc.want {
				t.Fatalf("got %s (%v), want %s", j, err, tc.want)
			}
		})
	}
}
