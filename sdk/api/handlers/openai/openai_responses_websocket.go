package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsRequestTypeCreate           = "response.create"
	wsRequestTypeAppend           = "response.append"
	wsEventTypeError              = "error"
	wsEventTypeCompleted          = "response.completed"
	wsDoneMarker                  = "[DONE]"
	wsTurnStateHeader             = "x-codex-turn-state"
	wsRequestBodyKey              = "REQUEST_BODY_OVERRIDE"
	wsTimelineBodyKey             = "WEBSOCKET_TIMELINE_OVERRIDE"
	wsRetryableErrorType          = "invalid_request_error"
	wsRetryableErrorCode          = "websocket_connection_limit_reached"
	wsRetryableErrorMsg           = "Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."
	responsesWebsocketIdleTimeout = 600 * time.Second
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	passthroughSessionID := uuid.NewString()
	downstreamSessionKey := websocketDownstreamSessionKey(c.Request)
	retainResponsesWebsocketToolCaches(downstreamSessionKey)
	clientIP := websocketClientAddress(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientIP)
	var wsTerminateErr error
	var wsTimelineLog strings.Builder
	defer func() {
		releaseResponsesWebsocketToolCaches(downstreamSessionKey)
		if wsTerminateErr != nil {
			appendWebsocketTimelineDisconnect(&wsTimelineLog, wsTerminateErr, time.Now())
			log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		setWebsocketTimelineBody(c, wsTimelineLog.String())
		if errClose := conn.Close(); errClose != nil {
			log.Warnf("responses websocket: close connection error: %v", errClose)
		}
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	pinnedAuthID := ""
	anyOperationCompleted := false
	maxBootstrapRetries := handlers.StreamingBootstrapRetries(h.Cfg)
	if maxBootstrapRetries < 1 {
		maxBootstrapRetries = 1
	}

	for {
		_ = conn.SetReadDeadline(time.Now().Add(responsesWebsocketIdleTimeout))
		msgType, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			var netErr net.Error
			if errors.As(errReadMessage, &netErr) && netErr.Timeout() {
				log.Infof("responses websocket: idle timeout id=%s timeout=%s", passthroughSessionID, responsesWebsocketIdleTimeout)
				return
			}
			if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		appendWebsocketTimelineEvent(&wsTimelineLog, "request", payload, time.Now())

		allowIncrementalInputWithPreviousResponseID := false
		if pinnedAuthID != "" && h != nil && h.AuthManager != nil {
			if pinnedAuth, ok := h.AuthManager.GetByID(pinnedAuthID); ok && pinnedAuth != nil {
				allowIncrementalInputWithPreviousResponseID = websocketUpstreamSupportsIncrementalInput(pinnedAuth.Attributes, pinnedAuth.Metadata)
			}
		} else {
			requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
			if requestModelName == "" {
				requestModelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
			}
			allowIncrementalInputWithPreviousResponseID = h.websocketUpstreamSupportsIncrementalInputForModel(requestModelName)
		}

		requestJSON, updatedLastRequest, errMsg := normalizeResponsesWebsocketRequestWithMode(payload, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, &wsTimelineLog, errMsg)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf("responses websocket: downstream_out write failed id=%s event=%s error=%v", passthroughSessionID, websocketPayloadEventType(errorPayload), errWrite)
				return
			}
			continue
		}
		if shouldHandleResponsesWebsocketPrewarmLocally(payload, lastRequest, allowIncrementalInputWithPreviousResponseID) {
			if updated, errDelete := sjson.DeleteBytes(requestJSON, "generate"); errDelete == nil {
				requestJSON = updated
			}
			if updated, errDelete := sjson.DeleteBytes(updatedLastRequest, "generate"); errDelete == nil {
				updatedLastRequest = updated
			}
			lastRequest = updatedLastRequest
			lastResponseOutput = []byte("[]")
			if errWrite := writeResponsesWebsocketSyntheticPrewarm(c, conn, requestJSON, &wsTimelineLog, passthroughSessionID); errWrite != nil {
				wsTerminateErr = errWrite
				return
			}
			continue
		}

		requestJSON = repairResponsesWebsocketToolCalls(downstreamSessionKey, requestJSON)
		updatedLastRequest = bytes.Clone(requestJSON)
		lastRequest = updatedLastRequest

		modelName := gjson.GetBytes(requestJSON, "model").String()
		retryDownstream := !anyOperationCompleted
		maxAttempts := 1
		if retryDownstream {
			maxAttempts = 1 + maxBootstrapRetries
		}
		for attempt := 0; attempt < maxAttempts; attempt++ {
			cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
			cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
			cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
			if pinnedAuthID != "" {
				cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
			} else {
				cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
					authID = strings.TrimSpace(authID)
					if authID == "" || h == nil || h.AuthManager == nil {
						return
					}
					selectedAuth, ok := h.AuthManager.GetByID(authID)
					if !ok || selectedAuth == nil {
						return
					}
					if websocketUpstreamSupportsIncrementalInput(selectedAuth.Attributes, selectedAuth.Metadata) {
						pinnedAuthID = authID
					}
				})
			}
			dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, requestJSON, "")

			allowRetry := retryDownstream && attempt < maxAttempts-1
			terminateOnForceReconnect := !retryDownstream
			completedOutput, completed, errForward := h.forwardResponsesWebsocket(c, conn, cliCancel, dataChan, errChan, &wsTimelineLog, passthroughSessionID, allowRetry, terminateOnForceReconnect)
			if errForward != nil {
				var retryErr *responsesWebsocketRetryError
				if allowRetry && errors.As(errForward, &retryErr) {
					if h != nil && h.AuthManager != nil {
						h.AuthManager.CloseExecutionSession(passthroughSessionID)
						log.Infof("responses websocket: downstream reconnect attempt=%d id=%s reason=%s error=%v", attempt+1, passthroughSessionID, retryErr.reason, retryErr.cause)
					}
					time.Sleep(responsesWebsocketRetryDelay(attempt))
					continue
				}
				wsTerminateErr = errForward
				log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
				return
			}
			if completed {
				anyOperationCompleted = true
			}
			lastResponseOutput = completedOutput
			break
		}
	}
}

func websocketClientAddress(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.ClientIP())
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
	case wsRequestTypeAppend:
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, allowIncrementalInputWithPreviousResponseID)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("unsupported websocket request type: %s", requestType)}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}
	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("missing model in response.create request")}
	}
	return normalized, bytes.Clone(normalized), nil
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("websocket request received before response.create")}
	}
	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, lastRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("websocket request requires array field: input")}
	}

	// Compaction can cause clients to replace local websocket history with a new
	// compact transcript on the next `response.create`. When the input already
	// contains historical model output items, treating it as an incremental append
	// duplicates stale turn-state and can leave late orphaned function_call items.
	if shouldReplaceWebsocketTranscript(rawJSON, nextInput) {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, bytes.Clone(normalized), nil
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		if prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = bytes.Clone(rawJSON)
			}
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			return normalized, bytes.Clone(normalized), nil
		}
	}
	existingInput := gjson.GetBytes(lastRequest, "input")
	mergedInput, errMerge := mergeJSONArrayRaw(existingInput.Raw, normalizeJSONArrayRaw(lastResponseOutput))
	if errMerge != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("invalid previous response output: %w", errMerge)}
	}
	mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, nextInput.Raw)
	if errMerge != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("invalid request input: %w", errMerge)}
	}
	dedupedInput, errDedupeFunctionCalls := dedupeFunctionCallsByCallID(mergedInput)
	if errDedupeFunctionCalls == nil {
		mergedInput = dedupedInput
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("failed to merge websocket input: %w", errSet)}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return normalized, bytes.Clone(normalized), nil
}

func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) != "" {
		return false
	}
	if !nextInput.Exists() || !nextInput.IsArray() {
		return false
	}

	for _, item := range nextInput.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call":
			return true
		case "message":
			role := strings.TrimSpace(item.Get("role").String())
			if role == "assistant" {
				return true
			}
		}
	}

	return false
}

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	return bytes.Clone(normalized)
}

func dedupeFunctionCallsByCallID(rawArray string) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}
	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	seenCallIDs := make(map[string]struct{}, len(items))
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		if isResponsesToolCallType(itemType) {
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID != "" {
				if _, ok := seenCallIDs[callID]; ok {
					continue
				}
				seenCallIDs[callID] = struct{}{}
			}
		}
		filtered = append(filtered, item)
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	if h == nil || h.AuthManager == nil {
		return false
	}
	resolvedModelName := modelName
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		} else {
			resolvedModelName = resolvedBase
		}
	} else {
		resolvedModelName = util.ResolveAutoModel(modelName)
	}
	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	providers := util.GetProviderName(baseModel)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}
	if len(providers) == 0 {
		return false
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey != "" {
			providerSet[providerKey] = struct{}{}
		}
	}
	if len(providerSet) == 0 {
		return false
	}
	modelKey := baseModel
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	for _, auth := range h.AuthManager.List() {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(auth.ID, modelKey) {
			continue
		}
		if !responsesWebsocketAuthAvailableForModel(auth, modelKey, now) {
			continue
		}
		if websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata) {
			return true
		}
	}
	return false
}

func responsesWebsocketAuthAvailableForModel(auth *coreauth.Auth, modelName string, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if modelName != "" && len(auth.ModelStates) > 0 {
		state, ok := auth.ModelStates[modelName]
		if (!ok || state == nil) && modelName != "" {
			baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
			if baseModel != "" && baseModel != modelName {
				state, ok = auth.ModelStates[baseModel]
			}
		}
		if ok && state != nil {
			if state.Status == coreauth.StatusDisabled {
				return false
			}
			if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
				return false
			}
			return true
		}
	}
	if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	return true
}

func shouldHandleResponsesWebsocketPrewarmLocally(rawJSON []byte, lastRequest []byte, allowIncrementalInputWithPreviousResponseID bool) bool {
	if allowIncrementalInputWithPreviousResponseID || len(lastRequest) != 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	generateResult := gjson.GetBytes(rawJSON, "generate")
	return generateResult.Exists() && !generateResult.Bool()
}

func writeResponsesWebsocketSyntheticPrewarm(
	c *gin.Context,
	conn *websocket.Conn,
	requestJSON []byte,
	wsTimelineLog *strings.Builder,
	sessionID string,
) error {
	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads(requestJSON)
	if errPayloads != nil {
		return errPayloads
	}
	for _, payload := range payloads {
		markAPIResponseTimestamp(c)
		if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now()); errWrite != nil {
			log.Warnf("responses websocket: downstream_out write failed id=%s event=%s error=%v", sessionID, websocketPayloadEventType(payload), errWrite)
			return errWrite
		}
	}
	return nil
}

func syntheticResponsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, error) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())
	createdPayload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	var errSet error
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		createdPayload, errSet = sjson.SetBytes(createdPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}
	completedPayload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		completedPayload, errSet = sjson.SetBytes(completedPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}
	return [][]byte{createdPayload, completedPayload}, nil
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw = strings.TrimSpace(existingRaw)
	appendRaw = strings.TrimSpace(appendRaw)
	if existingRaw == "" {
		existingRaw = "[]"
	}
	if appendRaw == "" {
		appendRaw = "[]"
	}
	var existing []json.RawMessage
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return "", err
	}
	var appendItems []json.RawMessage
	if err := json.Unmarshal([]byte(appendRaw), &appendItems); err != nil {
		return "", err
	}
	merged := append(existing, appendItems...)
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog *strings.Builder,
	sessionID string,
	allowRetry bool,
	terminateOnForceReconnect bool,
) ([]byte, bool, error) {
	completed := false
	completedOutput := []byte("[]")
	sentPayload := false
	downstreamSessionKey := ""
	if c != nil && c.Request != nil {
		downstreamSessionKey = websocketDownstreamSessionKey(c.Request)
	}
	idleTimer := time.NewTimer(responsesWebsocketIdleTimeout)
	defer func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
	}()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(responsesWebsocketIdleTimeout)
	}
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return completedOutput, completed, c.Request.Context().Err()
		case <-idleTimer.C:
			errIdle := fmt.Errorf("responses websocket: idle timeout (%s)", responsesWebsocketIdleTimeout)
			cancel(errIdle)
			return completedOutput, completed, errIdle
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			resetIdle()
			forceReconnect := shouldTerminateResponsesWebsocketOnError(errMsg)
			if errMsg != nil {
				if allowRetry && !sentPayload && responsesWebsocketShouldRetryDownstream(errMsg, forceReconnect) {
					cancel(errMsg.Error)
					return completedOutput, completed, &responsesWebsocketRetryError{cause: errMsg.Error, reason: "downstream_error"}
				}
				if errMsg.StatusCode >= http.StatusBadRequest && errMsg.StatusCode < http.StatusInternalServerError {
					log.Warnf("responses websocket: intercepted 4XX error id=%s status=%d error=%v (force shutdown to trigger client retry)", sessionID, errMsg.StatusCode, errMsg.Error)
					h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
					markAPIResponseTimestamp(c)
					cancel(errMsg.Error)
					conn.UnderlyingConn().Close()
					return completedOutput, completed, fmt.Errorf("responses websocket: 4XX intercepted status=%d", errMsg.StatusCode)
				}
				h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
				markAPIResponseTimestamp(c)
				var errorPayload []byte
				var errWrite error
				if forceReconnect {
					errorPayload, errWrite = writeResponsesWebsocketRetryableReconnectError(conn, wsTimelineLog, errMsg)
				} else {
					errorPayload, errWrite = writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
				}
				if errWrite == nil {
					sentPayload = true
					resetIdle()
				}
				log.Infof("responses websocket: downstream_out id=%s type=%d event=%s payload=%s", sessionID, websocket.TextMessage, websocketPayloadEventType(errorPayload), websocketPayloadPreview(errorPayload))
				if errWrite != nil {
					cancel(errMsg.Error)
					return completedOutput, completed, errWrite
				}
			}
			if errMsg != nil {
				cancel(errMsg.Error)
				if forceReconnect {
					if errMsg.Error != nil {
						if terminateOnForceReconnect {
							return completedOutput, completed, errMsg.Error
						}
						return completedOutput, completed, nil
					}
					if terminateOnForceReconnect {
						return completedOutput, completed, fmt.Errorf("responses websocket: force reconnect on usage limit")
					}
					return completedOutput, completed, nil
				}
			} else {
				cancel(nil)
			}
			return completedOutput, completed, nil
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: fmt.Errorf("stream closed before response.completed")}
					if allowRetry && !sentPayload {
						cancel(errMsg.Error)
						return completedOutput, completed, &responsesWebsocketRetryError{cause: errMsg.Error, reason: "stream_closed"}
					}
					h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
					markAPIResponseTimestamp(c)
					errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
					if errWrite == nil {
						sentPayload = true
						resetIdle()
					}
					log.Infof("responses websocket: downstream_out id=%s type=%d event=%s payload=%s", sessionID, websocket.TextMessage, websocketPayloadEventType(errorPayload), websocketPayloadPreview(errorPayload))
					if errWrite != nil {
						log.Warnf("responses websocket: downstream_out write failed id=%s event=%s error=%v", sessionID, websocketPayloadEventType(errorPayload), errWrite)
						cancel(errMsg.Error)
						return completedOutput, completed, errWrite
					}
					cancel(errMsg.Error)
					return completedOutput, completed, nil
				}
				cancel(nil)
				return completedOutput, completed, nil
			}
			resetIdle()
			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				recordResponsesWebsocketToolCallsFromPayload(downstreamSessionKey, payloads[i])
				eventType := gjson.GetBytes(payloads[i], "type").String()
				if eventType == wsEventTypeCompleted {
					completed = true
					completedOutput = responseCompletedOutputFromPayload(payloads[i])
				}
				markAPIResponseTimestamp(c)
				if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
					log.Warnf("responses websocket: downstream_out write failed id=%s event=%s error=%v", sessionID, websocketPayloadEventType(payloads[i]), errWrite)
					cancel(errWrite)
					return completedOutput, completed, errWrite
				}
				sentPayload = true
			}
		}
	}
}

type responsesWebsocketRetryError struct {
	cause  error
	reason string
}

func (e *responsesWebsocketRetryError) Error() string {
	if e == nil || e.cause == nil {
		return "responses websocket: retryable downstream error"
	}
	if e.reason != "" {
		return fmt.Sprintf("responses websocket: retryable downstream error (%s): %v", e.reason, e.cause)
	}
	return fmt.Sprintf("responses websocket: retryable downstream error: %v", e.cause)
}

func (e *responsesWebsocketRetryError) Unwrap() error { return e.cause }

func responsesWebsocketRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 250 * time.Millisecond
	}
	if attempt == 1 {
		return 500 * time.Millisecond
	}
	if attempt == 2 {
		return time.Second
	}
	return 2 * time.Second
}

func responsesWebsocketShouldRetryDownstream(errMsg *interfaces.ErrorMessage, forceReconnect bool) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	if forceReconnect {
		return true
	}
	status := errMsg.StatusCode
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return true
	}
	if status == 0 || status == http.StatusBadGateway || status == http.StatusGatewayTimeout || status >= http.StatusInternalServerError {
		return true
	}
	return responsesWebsocketIsLikelyDisconnect(errMsg.Error)
}

func responsesWebsocketIsLikelyDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe") || strings.Contains(lower, "use of closed network connection") || strings.Contains(lower, "websocket: close") || strings.Contains(lower, "write:") && strings.Contains(lower, "connection") && strings.Contains(lower, "closed")
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return bytes.Clone([]byte(output.Raw))
	}
	return []byte("[]")
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for _, rawLine := range lines {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, bytes.Clone(line))
		}
	}
	if len(payloads) > 0 {
		return payloads
	}
	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, bytes.Clone(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketRetryableReconnectError(conn *websocket.Conn, wsTimelineLog *strings.Builder, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	payload := buildResponsesWebsocketRetryableReconnectPayload(errMsg)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, writeResponsesWebsocketPayload(conn, wsTimelineLog, data, time.Now())
}

func buildResponsesWebsocketRetryableReconnectPayload(errMsg *interfaces.ErrorMessage) map[string]any {
	payload := map[string]any{"type": wsEventTypeError, "status": http.StatusBadRequest, "error": map[string]any{"type": wsRetryableErrorType, "code": wsRetryableErrorCode, "message": usageLimitErrorMessage(errMsg)}}
	appendResponsesWebsocketErrorHeaders(payload, errMsg)
	return payload
}

func writeResponsesWebsocketError(conn *websocket.Conn, wsTimelineLog *strings.Builder, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
		}
		if errMsg.Error != nil {
			errText = errMsg.Error.Error()
		}
	}
	body := handlers.BuildErrorResponseBody(status, errText)
	payload := map[string]any{"type": wsEventTypeError, "status": status}
	appendResponsesWebsocketErrorHeaders(payload, errMsg)
	if len(body) > 0 && json.Valid(body) {
		decoded := map[string]any{}
		if err := json.Unmarshal(body, &decoded); err == nil {
			if inner, ok := decoded["error"]; ok {
				payload["error"] = inner
			} else {
				payload["error"] = decoded
			}
		}
	}
	if _, ok := payload["error"]; !ok {
		payload["error"] = map[string]any{"type": "server_error", "message": errText}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, writeResponsesWebsocketPayload(conn, wsTimelineLog, data, time.Now())
}

func appendResponsesWebsocketErrorHeaders(payload map[string]any, errMsg *interfaces.ErrorMessage) {
	if payload == nil || errMsg == nil || errMsg.Addon == nil {
		return
	}
	headers := map[string]any{}
	for key, values := range errMsg.Addon {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	if len(headers) > 0 {
		payload["headers"] = headers
	}
}

func appendWebsocketEvent(builder *strings.Builder, eventType string, payload []byte) {
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
}

func websocketPayloadPreview(payload []byte) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	previewText := strings.ReplaceAll(string(trimmedPayload), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	return previewText
}

func shouldTerminateResponsesWebsocketOnError(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	raw := errMsg.Error.Error()
	return isUsageLimitMessage(raw) || isMissingToolOutputMessage(raw)
}

func usageLimitErrorMessage(errMsg *interfaces.ErrorMessage) string {
	if errMsg == nil || errMsg.Error == nil {
		return wsRetryableErrorMsg
	}
	raw := strings.TrimSpace(errMsg.Error.Error())
	if raw == "" {
		return wsRetryableErrorMsg
	}
	if json.Valid([]byte(raw)) {
		msg := strings.TrimSpace(gjson.Get(raw, "error.message").String())
		if msg != "" {
			return msg
		}
	}
	return raw
}

func isUsageLimitMessage(message string) bool {
	raw := strings.TrimSpace(message)
	if raw == "" {
		return false
	}
	if json.Valid([]byte(raw)) {
		errType := strings.ToLower(strings.TrimSpace(gjson.Get(raw, "error.type").String()))
		if errType == "usage_limit_reached" {
			return true
		}
		status := int(gjson.Get(raw, "status").Int())
		errMsg := strings.ToLower(strings.TrimSpace(gjson.Get(raw, "error.message").String()))
		hasResetHint := gjson.Get(raw, "error.resets_at").Exists() || gjson.Get(raw, "error.resets_in_seconds").Exists()
		if status == http.StatusTooManyRequests && (strings.Contains(errMsg, "usage limit") || hasResetHint) {
			return true
		}
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "usage_limit_reached") || strings.Contains(lower, "\"status\":429") && strings.Contains(lower, "usage limit") || strings.Contains(lower, "usage limit") && strings.Contains(lower, "try again at") {
		return true
	}
	return false
}

func isMissingToolOutputMessage(message string) bool {
	raw := strings.TrimSpace(message)
	if raw == "" {
		return false
	}
	if json.Valid([]byte(raw)) {
		errMsg := strings.TrimSpace(gjson.Get(raw, "error.message").String())
		if errMsg != "" {
			return isMissingToolOutputText(errMsg)
		}
	}
	return isMissingToolOutputText(raw)
}

func isMissingToolOutputText(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "no tool output found for function call") || strings.Contains(lower, "no tool output found for tool call")
}

func setWebsocketRequestBody(c *gin.Context, body string) {
	setWebsocketBody(c, wsRequestBodyKey, body)
}

func setWebsocketTimelineBody(c *gin.Context, body string) {
	setWebsocketBody(c, wsTimelineBodyKey, body)
}

func setWebsocketBody(c *gin.Context, key string, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(key, []byte(trimmedBody))
}

func writeResponsesWebsocketPayload(conn *websocket.Conn, wsTimelineLog *strings.Builder, payload []byte, timestamp time.Time) error {
	appendWebsocketTimelineEvent(wsTimelineLog, "response", payload, timestamp)
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func appendWebsocketTimelineDisconnect(builder *strings.Builder, err error, timestamp time.Time) {
	if err == nil {
		return
	}
	appendWebsocketTimelineEvent(builder, "disconnect", []byte(err.Error()), timestamp)
}

func appendWebsocketTimelineEvent(builder *strings.Builder, eventType string, payload []byte, timestamp time.Time) {
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("Timestamp: ")
	builder.WriteString(timestamp.Format(time.RFC3339Nano))
	builder.WriteString("\n")
	builder.WriteString("Event: websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
