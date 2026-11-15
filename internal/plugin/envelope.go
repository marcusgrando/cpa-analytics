package plugin

import (
	"encoding/json"
	"errors"
)

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

// codedError carries a stable error code to the host envelope.
type codedError struct {
	code string
	msg  string
}

func (e *codedError) Error() string { return e.code + ": " + e.msg }

func asCoded(err error, target **codedError) bool { return errors.As(err, target) }

func okEnvelope(result []byte) []byte {
	if len(result) == 0 {
		result = []byte("{}")
	}
	raw, err := json.Marshal(wireEnvelope{OK: true, Result: result})
	if err != nil {
		return errorEnvelope("encode_error", err.Error())
	}
	return raw
}

func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(wireEnvelope{Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"encode_error","message":"failed to encode error"}}`)
	}
	return raw
}
