package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type responsesValidatedRequest struct {
	body                                                        []byte
	model, logModel                                             string
	mappingApplied, bodySignalCompact, nativeRemoteCompactionV2 bool
	handlerStart, bodyReadDone                                  time.Time
}

type traeCNResumeIdentity struct {
	key   string
	root  string
	input []string
}

func traeCNResumeDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// 只删除输出项的传输状态；正文、工具参数和调用标识必须参与核对。
func traeCNResumeItemDigest(raw json.RawMessage) string {
	var item map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&item) != nil {
		return traeCNResumeDigest(raw)
	}
	delete(item, "status")
	if _, ok := item["type"]; !ok && item["role"] != nil {
		item["type"] = "message"
	}
	if content, ok := item["content"].([]any); ok {
		for _, part := range content {
			if object, ok := part.(map[string]any); ok {
				if annotations, ok := object["annotations"].([]any); ok && len(annotations) == 0 {
					delete(object, "annotations")
				}
			}
		}
	}
	encoded, _ := json.Marshal(item)
	return traeCNResumeDigest(encoded)
}

func traeCNResumeRequestIdentity(c *gin.Context, body []byte) (traeCNResumeIdentity, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return traeCNResumeIdentity{}, false
	}
	var input []json.RawMessage
	if json.Unmarshal(root["input"], &input) != nil {
		return traeCNResumeIdentity{}, false
	}
	if len(input) > 8192 {
		return traeCNResumeIdentity{}, false
	}
	identity := traeCNResumeIdentity{}
	for _, item := range input {
		identity.input = append(identity.input, traeCNResumeItemDigest(item))
	}
	delete(root, "input")
	// 客户端重试会更新诊断元数据，业务参数则必须保持一致。
	delete(root, "client_metadata")
	encoded, _ := json.Marshal(root)
	identity.root = traeCNResumeDigest(encoded)

	metadata := gjson.GetBytes(body, "client_metadata")
	embedded := metadata.Get("x-codex-turn-metadata")
	if embedded.Type == gjson.String {
		embedded = gjson.Parse(embedded.String())
	}
	turnMeta := gjson.Parse(c.GetHeader(codexTurnMetadataHeader))
	thread := firstNonEmptyString(c.GetHeader("Thread-Id"), c.GetHeader("Thread_id"), metadata.Get("thread_id").String(), turnMeta.Get("thread_id").String(), embedded.Get("thread_id").String())
	turn := firstNonEmptyString(turnMeta.Get("turn_id").String(), embedded.Get("turn_id").String())
	requestID := strings.TrimSpace(c.GetHeader("X-NewAPI-Request-ID"))
	credential := downstreamCredentialKey(c.Request.Header)
	if credential == "" && requestAPIKeyID(c) <= 0 {
		return identity, false
	}
	owner := []string{fmt.Sprint(requestAPIKeyID(c)), traeCNResumeDigest([]byte(credential))}
	verified, _ := c.Get(newAPIIdentityContextKey)
	user, trusted := verified.(verifiedNewAPIIdentityContext)
	if trusted && user.APIKeyID == requestAPIKeyID(c) && user.Identity.UserID != "" {
		owner = append(owner, "user", user.Platform, user.Identity.UserID)
		if thread == "" || turn == "" {
			return identity, false
		}
	} else {
		// 共享渠道 Key 下没有可信用户身份时，只允许同一上游请求 ID 接回。
		if requestID == "" || len(requestID) > 256 || strings.ContainsAny(requestID, "\r\n") {
			return identity, false
		}
		owner = append(owner, "request", requestID)
	}
	if row := apiKeyRowFromContext(c); row != nil {
		scope, _ := json.Marshal(struct {
			Groups []int64
			Limits any
		}{row.AllowedGroupIDs, row.Limits})
		owner = append(owner, "scope", traeCNResumeDigest(scope))
	}
	if raw, ok := c.Get(newAPIPolicyMetaContextKey); ok {
		if policy, ok := raw.(verifiedNewAPIPolicyContext); ok && policy.MetaVerified {
			owner = append(owner, "channel", fmt.Sprint(policy.Meta.ChannelID))
		}
	}
	encoded, _ = json.Marshal(append(owner, thread, turn))
	identity.key = traeCNResumeDigest(encoded)
	return identity, true
}
