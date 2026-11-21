package station

import (
	"encoding/binary"
	"errors"
	"math"
	"unicode/utf8"

	"github.com/skrashevich/quasarc/protocol"
)

func (ff fields) text(id uint64) *string {
	f, ok := ff[id]
	if !ok || f.WT != 2 || !utf8.Valid(f.Payload) {
		return nil
	}
	s := string(f.Payload)
	return &s
}

func metricaUUID(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	uuid := ff.text(6)
	// metricad also publishes unrelated events with the same message ID.
	if uuid == nil || *uuid == "" {
		return nil, nil
	}
	return MetricaUUID{*uuid, ff.text(7)}, nil
}

// SDKState is field 34 of IOEvent. Other IO events share the subscription.
func sdkFields(f protocol.Frame) (fields, error) {
	event, err := unpack(f)
	if err != nil {
		return nil, err
	}
	sdk, ok := event[34]
	if !ok {
		return nil, nil
	}
	return unpack(sdk)
}

type UpdateStatus struct {
	State            *string  `json:"state"`
	Code             *int32   `json:"code"`
	IsCritical       *bool    `json:"is_critical"`
	DownloadProgress *float64 `json:"download_progress"`
	TargetVersion    *string  `json:"target_version"`
}

func sdkUpdateStatus(f protocol.Frame) (any, error) {
	sdk, err := sdkFields(f)
	if err != nil || sdk == nil {
		return nil, err
	}
	update, ok := sdk[7]
	if !ok {
		return UpdateStatus{}, nil
	}
	return updateStatus(update)
}

func updateStatus(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	result := UpdateStatus{TargetVersion: ff.text(4)}
	if code, ok := ff.number(1); ok {
		state := enumName(code, "", "none", "downloading", "applying")
		result.State = &state
		result.Code = &code
	}
	if v, ok := ff.number(2); ok {
		critical := v != 0
		result.IsCritical = &critical
	}
	if progress, ok := ff[3]; ok && progress.WT == 1 {
		v := math.Float64frombits(binary.LittleEndian.Uint64(progress.Payload))
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("invalid download_progress")
		}
		result.DownloadProgress = &v
	}
	return result, nil
}

type NotificationStatus struct {
	HasNotifications *bool   `json:"has_notifications"`
	Count            *int32  `json:"count"`
	Mode             *string `json:"mode"`
}

// Android ACTIVE and SDK AVAILABLE both indicate an available notification.
func (n *NotificationStatus) setMode(code int32) {
	label := enumName(code, "none", "available", "passive")
	n.Mode = &label
	if label != "unknown" {
		has := code != 0
		n.HasNotifications = &has
	}
}

func sdkNotificationStatus(f protocol.Frame) (any, error) {
	sdk, err := sdkFields(f)
	if err != nil || sdk == nil {
		return nil, err
	}
	var result NotificationStatus
	if code, ok := sdk.number(12); ok {
		result.setMode(code)
	}
	return result, nil
}

func notificationStatus(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	var result NotificationStatus
	if code, ok := ff.number(2); ok {
		result.setMode(code)
	}
	if count, ok := ff.number(1); ok {
		if count < 0 {
			return nil, errors.New("negative notification count")
		}
		has := count > 0
		result.Count = &count
		result.HasNotifications = &has
	}
	return result, nil
}

type MetricaUUID struct {
	UUID            string  `json:"uuid"`
	MetricaDeviceID *string `json:"metrica_device_id"`
}
