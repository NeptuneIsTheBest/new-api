package controller

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func configureResponsesWSInputFilter(t *testing.T, enabled bool) {
	t.Helper()
	source, err := plugins.Source("request-filter")
	require.NoError(t, err)
	registerChannelGapsPlugin(t, "ws-input-filter", strings.Replace(source, `key: "request-filter"`, `key: "%KEY%"`, 1))
	previous, err := common.Marshal(request_plugin_setting.GetInstances())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: string(previous)}))
	})
	instances := fmt.Sprintf(`[{"id":"ws-filter","name":"WS filter","plugin":"ws-input-filter","enabled":%t,"global":true,"config":{"keywords":"forbidden","message":"Blocked"}}]`, enabled)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: instances}))
}

// wsPlugin rejects a create that asks for "forbidden", redacts "secret" from
// its input, masks "bad" in the reply and stops a reply that says "stopword".
const wsPlugin = `export const meta = {apiVersion: 1, key: "rp-ws", name: "WS", version: "1.0.0", author: {name: "Test"}};
export function ingress(ctx, req) {
  const input = req.body.input;
  if (input.includes("forbidden")) return {action: "reject", code: "blocked.ws", message: "Blocked"};
  if (!input.includes("secret")) return {action: "continue"};
  return {action: "continue", effects: [{type: "json.patch", ops: [{op: "replace", path: "/input", value: input.replaceAll("secret", "[redacted]")}]}]};
}
export function reply(ctx, res) {
  const result = {action: "continue", mask: []};
  for (const output of res.outputs) {
    if (output.text.includes("stopword")) return {action: "stop", code: "reply.blocked"};
    for (let i = output.text.indexOf("bad"); i >= 0; i = output.text.indexOf("bad", i + 1)) {
      result.mask.push({index: output.index, start: i, end: i + 3, category: "word"});
    }
  }
  return result;
}`

// Each response.create on one connection runs the request plugins as an
// HTTP request does; a rejection or a stop ends only that response.
func TestResponsesWebSocketRunsRequestPlugins(t *testing.T) {
	require.NoError(t, i18n.Init())
	plugin, err := jsplugin.DefaultRegistry.Register(wsPlugin, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = jsplugin.DefaultRegistry.Unregister(plugin.Meta.Key) })
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: `[{"id":"ws","name":"WS","plugin":"rp-ws","enabled":true,"global":true}]`}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: "[]"}))
	})
	previousErrorLog := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = true
	t.Cleanup(func() { constant.ErrorLogEnabled = previousErrorLog })

	usage := `"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}`
	received := make(chan string, 8)
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		send := func(event string) bool {
			return assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event)))
		}
		for index := 1; ; index++ {
			_, create, err := ws.ReadMessage()
			if err != nil {
				return
			}
			received <- string(create)
			id := fmt.Sprintf("resp_%d", index)
			if !send(`{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`) {
				return
			}
			text := gjson.GetBytes(create, "input").Str
			switch {
			case strings.Contains(text, "[redacted]"):
				for _, delta := range []string{"a bad ", "word"} {
					if !send(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"` + delta + `"}`) {
						return
					}
				}
				send(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"a bad word"}]}],` + usage + `}}`)
			case strings.Contains(text, "stop"):
				if !send(`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"say stopword now"}`) {
					return
				}
				_, cancel, err := ws.ReadMessage()
				if err != nil {
					return
				}
				received <- string(cancel)
				send(`{"type":"response.cancelled","response":{"id":"` + id + `","status":"cancelled",` + usage + `}}`)
			default:
				send(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],` + usage + `}}`)
			}
		}
	})
	client := fixture.client
	create := func(input string) {
		require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"`+input+`"}`)))
	}
	upstream := func() string {
		select {
		case message := <-received:
			return message
		case <-time.After(3 * time.Second):
			t.Fatal("upstream received nothing")
			return ""
		}
	}

	// A rejected create never reaches the upstream.
	create("a forbidden question")
	rejected := readResponsesWSTestEvent(t, client)
	assert.Equal(t, "error", rejected["type"])
	assert.Equal(t, float64(http.StatusBadRequest), rejected["status"])
	assert.Equal(t, "request_rejected", rejected["error"].(map[string]any)["code"])
	assert.Empty(t, received)

	// The ingress patch reaches the upstream and the reply is masked.
	create("my secret plan")
	assert.Equal(t, "my [redacted] plan", gjson.Get(upstream(), "input").Str)
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, client)["type"])
	var deltas []any
	for range 2 {
		event := readResponsesWSTestEvent(t, client)
		assert.Equal(t, "response.output_text.delta", event["type"])
		deltas = append(deltas, event["delta"])
	}
	assert.Equal(t, []any{"a *** ", "word"}, deltas)
	completed := readResponsesWSTestEvent(t, client)
	assert.Equal(t, "response.completed", completed["type"])
	encoded, err := common.Marshal(completed)
	require.NoError(t, err)
	assert.Equal(t, "a *** word", gjson.GetBytes(encoded, "response.output.0.content.0.text").Str)

	// A stop cancels the upstream response; the client gets the error. The
	// rejected create never reached the upstream, so this is its second.
	create("please stop")
	upstream()
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, client)["type"])
	cancel := upstream()
	assert.Equal(t, "response.cancel", gjson.Get(cancel, "type").Str)
	assert.Equal(t, "resp_2", gjson.Get(cancel, "response_id").Str)
	stopped := readResponsesWSTestEvent(t, client)
	assert.Equal(t, "error", stopped["type"])
	assert.Equal(t, float64(http.StatusBadRequest), stopped["status"])
	assert.Equal(t, "resp_2", stopped["response_id"])
	assert.Equal(t, "response_rejected", stopped["error"].(map[string]any)["code"])

	// The connection still serves the next create.
	create("hello")
	upstream()
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, client)["type"])
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, client)["type"])

	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{1000, 1000, 1000})
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	for index, want := range []string{
		`[{"plugin":"rp-ws","stage":"ingress","changes":[{"op":"replace","path":"input","value":"my [redacted] plan"}]},{"plugin":"rp-ws","stage":"reply","masked":1,"findings":[{"category":"word","count":1}]}]`,
		`[{"plugin":"rp-ws","stage":"reply","rejected":{"code":"reply.blocked"}}]`,
		``,
	} {
		record := gjson.Get(logs[index].Other, "request_plugins")
		if want == "" {
			assert.False(t, record.Exists(), logs[index].Other)
			continue
		}
		assert.JSONEq(t, want, stripRecordNames(t, record.Raw), logs[index].Other)
	}
	var errorLogs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeError).Find(&errorLogs).Error)
	require.Len(t, errorLogs, 1)
	assert.Contains(t, errorLogs[0].Content, "Blocked")
	assert.JSONEq(t, `[{"plugin":"rp-ws","stage":"ingress","rejected":{"code":"blocked.ws"}}]`, stripRecordNames(t, gjson.Get(errorLogs[0].Other, "request_plugins").Raw))
}

// stripRecordNames drops the display names, which come from the plugin meta.
func stripRecordNames(t *testing.T, raw string) string {
	t.Helper()
	var entries []map[string]any
	require.NoError(t, common.UnmarshalJsonStr(raw, &entries))
	for _, entry := range entries {
		delete(entry, "name")
	}
	encoded, err := common.Marshal(entries)
	require.NoError(t, err)
	return string(encoded)
}

func TestResponsesWebSocketControlPluginsRewriteEachInputOnce(t *testing.T) {
	const source = `export const meta = {apiVersion: 1, key: "%KEY%", name: "Input", version: "1.0.0", author: {name: "Test"}};
export function ingress(ctx, req) {
  const input = req.body.input;
  if (JSON.stringify(input).includes("forbidden")) return {action: "reject", code: "blocked.input"};
  const value = typeof input === "string" ? "checked:" + input : input.map(item => ({...item, content: "checked:" + item.content}));
  return {action: "continue", effects: [{type: "json.patch", ops: [{op: "replace", path: "/input", value}]}]};
}`
	for _, control := range []string{"response.inject", "response.steer"} {
		for _, bound := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/channel=%t", control, bound), func(t *testing.T) {
				registerChannelGapsPlugin(t, "ws-control-input", source)
				previous, err := common.Marshal(request_plugin_setting.GetInstances())
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: string(previous)}))
				})
				instances := fmt.Sprintf(`[{"id":"control-input","name":"Input","plugin":"ws-control-input","enabled":true,"global":%t}]`, !bound)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{request_plugin_setting.InstancesOption: instances}))
				fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, _ *http.Request) {
					create := readResponsesWSTestEvent(t, ws)
					assert.Equal(t, "checked:start", create["input"])
					sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "lane", "response": map[string]any{"id": "parent", "status": "in_progress"}})
					for index, input := range []string{"first", "second"} {
						event := readResponsesWSTestEvent(t, ws)
						assert.Equal(t, control, event["type"], "rejected input must not reach upstream")
						if control == "response.inject" {
							items, ok := event["input"].([]any)
							require.True(t, ok)
							require.Len(t, items, 1)
							assert.Equal(t, "checked:"+input, items[0].(map[string]any)["content"])
							sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.inject.created", "response_id": "parent", "event_id": fmt.Sprintf("input-%d", index)})
						} else {
							assert.Equal(t, "checked:"+input, event["input"])
							sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.steer.accepted", "steer": map[string]any{"id": fmt.Sprintf("steer-%d", index), "previous_response_id": "parent"}})
						}
					}
					completeResponsesWSTestRequest(t, ws, "lane", "parent", 10)
					if control == "response.steer" {
						sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "stream_id": "lane", "response": map[string]any{"id": "child", "previous_response_id": "parent", "status": "in_progress"}})
						completeResponsesWSTestRequest(t, ws, "lane", "child", 20)
					}
					_, _, _ = ws.ReadMessage()
				})
				if bound {
					settings := fixture.channel.GetSetting()
					settings.RequestPlugins = []string{"control-input"}
					encoded, err := common.Marshal(settings)
					require.NoError(t, err)
					require.NoError(t, model.DB.Model(fixture.channel).Update("setting", string(encoded)).Error)
				}
				sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "stream_id": "lane", "model": "ws-billing", "input": "start"})
				require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
				for index, input := range []string{"forbidden", "first", "second"} {
					event := map[string]any{"type": control, "previous_response_id": "parent", "input": input}
					if control == "response.inject" {
						delete(event, "previous_response_id")
						event["response_id"] = "parent"
						event["event_id"] = fmt.Sprintf("input-%d", index-1)
						event["input"] = []any{map[string]any{"role": "user", "content": input}}
					}
					sendResponsesWSTestEvent(t, fixture.client, event)
					reply := readResponsesWSTestEvent(t, fixture.client)
					if index == 0 {
						require.Equal(t, "error", reply["type"])
						assert.Equal(t, "request_rejected", reply["error"].(map[string]any)["code"])
					} else if control == "response.inject" {
						require.Equal(t, "response.inject.created", reply["type"])
					} else {
						require.Equal(t, "response.steer.accepted", reply["type"])
					}
				}
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				quotas := []int{100}
				if control == "response.steer" {
					require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
					require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
					quotas = append(quotas, 100)
				}
				fixture.closeAndWait(t)
				assertResponsesWSAccounting(t, fixture, quotas)
			})
		}
	}
}

func TestResponsesWebSocketProviderSignatureKeepsConnection(t *testing.T) {
	registerChannelGapsPlugin(t, "ws-signed-provider", `export const meta = {
apiVersion: 1, key: "%KEY%", name: "Signed WS", version: "1.0.0", author: {name: "Test"},
models: ["ws-billing", "gpt-4o"], relay: [{incoming_path: "/v1/responses", upstream_path: "/v1/responses", converter: "none"}]
};
export function signRequest() { return {headers: {"X-Request-Signature": utils.uuid()}}; }
`)
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.0002))`, func(ws *websocket.Conn, request *http.Request) {
		assert.NotEmpty(t, request.Header.Get("X-Request-Signature"))
		for _, id := range []string{"first", "second"} {
			create := readResponsesWSTestEvent(t, ws)
			assert.Equal(t, "response.create", create["type"])
			sendResponsesWSTestEvent(t, ws, map[string]any{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress"}})
			completeResponsesWSTestRequest(t, ws, "", id, 5)
		}
		_, _, _ = ws.ReadMessage()
	})
	settings := fixture.channel.GetSetting()
	settings.TaskPluginKey = "ws-signed-provider"
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	fixture.channel.Type = constant.ChannelTypeTaskPlugin
	fixture.channel.Setting = common.GetPointer(string(encoded))
	require.NoError(t, fixture.channel.Update())
	for range 2 {
		sendResponsesWSTestEvent(t, fixture.client, map[string]any{"type": "response.create", "model": "ws-billing", "input": "hello"})
		require.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
		require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	}
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int{100, 100})
}
