package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	// devinGetCliModelConfigsPath is the Connect-RPC unary endpoint that lists
	// the upstream CLI model catalog (ported from cmd/fetch_devin_models).
	devinGetCliModelConfigsPath = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"
	devinDefaultBaseURL         = "https://server.codeium.com"
	devinDefaultUserAgent       = "connect-go/1.19.1 (go1.25.0)"
	devinClientName             = "chisel"
	devinClientVersion          = "3000.10.21"
	devinFingerprintHexLen      = 732
	maxModelConfigsResponseSize = 8 << 20
)

// rawDevinModel is one raw upstream model config row (pre-aggregation).
type rawDevinModel struct {
	UID           string
	Label         string
	ContextLength int
	Multimodal    bool
	VendorID      uint64
	EffortTier    string
}

// generateDeviceFingerprint ports internal/auth/devin.GenerateDeviceFingerprint:
// a 732-hex-char fingerprint. Empty seed -> cryptographically random.
func generateDeviceFingerprint(seed string) string {
	if seed == "" {
		var b [devinFingerprintHexLen / 2]byte
		if _, err := rand.Read(b[:]); err == nil {
			return hex.EncodeToString(b[:])
		}
		seed = fmt.Sprintf("cpa-devin-live-models-%d", time.Now().UnixNano())
	}
	var sb strings.Builder
	counter := 0
	for sb.Len() < devinFingerprintHexLen {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed, counter)))
		sb.WriteString(hex.EncodeToString(h[:]))
		counter++
	}
	return sb.String()[:devinFingerprintHexLen]
}

// buildDevinClientMetadataBytes ports helps.BuildDevinClientMetadataBytes.
func buildDevinClientMetadataBytes(sessionToken, deviceSeed, osName string) []byte {
	if osName == "" {
		osName = runtime.GOOS
	}
	fingerprint := generateDeviceFingerprint(deviceSeed)

	var meta []byte
	meta = protowire.AppendTag(meta, 1, protowire.BytesType)
	meta = protowire.AppendString(meta, devinClientName)
	meta = protowire.AppendTag(meta, 2, protowire.BytesType)
	meta = protowire.AppendString(meta, devinClientVersion)
	meta = protowire.AppendTag(meta, 3, protowire.BytesType)
	meta = protowire.AppendString(meta, sessionToken)
	meta = protowire.AppendTag(meta, 4, protowire.BytesType)
	meta = protowire.AppendString(meta, "en")
	meta = protowire.AppendTag(meta, 5, protowire.BytesType)
	meta = protowire.AppendString(meta, osName)
	meta = protowire.AppendTag(meta, 7, protowire.BytesType)
	meta = protowire.AppendString(meta, devinClientVersion)
	meta = protowire.AppendTag(meta, 12, protowire.BytesType)
	meta = protowire.AppendString(meta, devinClientName)
	meta = protowire.AppendTag(meta, 31, protowire.BytesType)
	meta = protowire.AppendString(meta, fingerprint)
	return meta
}

// buildGetCliModelConfigsRequest wraps client metadata in the outer request
// envelope (field 1, bytes).
func buildGetCliModelConfigsRequest(sessionToken, deviceSeed, osName string) []byte {
	meta := buildDevinClientMetadataBytes(sessionToken, deviceSeed, osName)
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, meta)
	return body
}

// fetchRawDevinModels posts GetCliModelConfigs and decodes the raw model list.
func fetchRawDevinModels(ctx context.Context, client *http.Client, baseURL, sessionToken, deviceSeed, osName string) ([]rawDevinModel, error) {
	if client == nil {
		client = http.DefaultClient
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = devinDefaultBaseURL
	}
	body := buildGetCliModelConfigsRequest(sessionToken, deviceSeed, osName)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, base+devinGetCliModelConfigsPath, bytes.NewReader(body))
	if errReq != nil {
		return nil, fmt.Errorf("create request: %w", errReq)
	}
	req.Header.Set("Authorization", "Basic "+sessionToken+"-"+sessionToken)
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("User-Agent", devinDefaultUserAgent)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("do request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, maxModelConfigsResponseSize))
	if errRead != nil {
		return nil, fmt.Errorf("read response body: %w", errRead)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, redactSnippet(string(respBytes)))
	}

	return parseRawDevinModelsProto(respBytes)
}

func redactSnippet(s string) string {
	const maxLen = 256
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

func parseRawDevinModelsProto(b []byte) ([]rawDevinModel, error) {
	var results []rawDevinModel
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, errors.New("malformed protobuf tag")
		}
		b = b[n:]

		if num == 1 && typ == protowire.BytesType {
			subBytes, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, errors.New("malformed protobuf submessage")
			}
			b = b[m:]
			model := parseSingleModelConfig(subBytes)
			if model.UID != "" {
				results = append(results, model)
			}
		} else {
			skip := protowire.ConsumeFieldValue(num, typ, b)
			if skip < 0 {
				return nil, errors.New("failed to skip field")
			}
			b = b[skip:]
		}
	}
	return results, nil
}

func parseSingleModelConfig(b []byte) rawDevinModel {
	var m rawDevinModel
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]

		switch num {
		case 1: // label
			if typ == protowire.BytesType {
				val, mLen := protowire.ConsumeString(b)
				if mLen >= 0 {
					m.Label = val
					b = b[mLen:]
					continue
				}
			}
		case 5: // multimodal
			if typ == protowire.VarintType {
				val, mLen := protowire.ConsumeVarint(b)
				if mLen >= 0 {
					m.Multimodal = val == 1
					b = b[mLen:]
					continue
				}
			}
		case 10: // vendor
			if typ == protowire.VarintType {
				val, mLen := protowire.ConsumeVarint(b)
				if mLen >= 0 {
					m.VendorID = val
					b = b[mLen:]
					continue
				}
			}
		case 18: // context length
			if typ == protowire.VarintType {
				val, mLen := protowire.ConsumeVarint(b)
				if mLen >= 0 {
					m.ContextLength = int(val)
					b = b[mLen:]
					continue
				}
			}
		case 22: // chat_model_uid
			if typ == protowire.BytesType {
				val, mLen := protowire.ConsumeString(b)
				if mLen >= 0 {
					m.UID = val
					b = b[mLen:]
					continue
				}
			}
		}

		skip := protowire.ConsumeFieldValue(num, typ, b)
		if skip < 0 {
			break
		}
		b = b[skip:]
	}
	return m
}
