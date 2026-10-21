// Package eventscontract extracts a bounded, source-backed subset of AsyncAPI
// 3.1.0 JSON. It never performs I/O or interprets runtime delivery guarantees.
package eventscontract

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	schema "github.com/LonelyTraderBay/maps-context-engine-4d/internal/contracts/schema"
	"github.com/LonelyTraderBay/maps-context-engine-4d/internal/model"
)

const (
	AsyncAPI31              = "3.1.0"
	DefaultSchemaFormat     = "application/vnd.aai.asyncapi+json;version=3.1.0"
	JSONSchemaDraft07Format = "application/schema+json;version=draft-07"
	maxAddressBytes         = 1024
)

type Action string

const (
	ActionSend    Action = "send"
	ActionReceive Action = "receive"
)

type AddressKind string

const (
	AddressLiteral  AddressKind = "literal"
	AddressTemplate AddressKind = "template"
	AddressUnknown  AddressKind = "unknown"
)

type DeclarationStatus string

const BindingDeclaredOnly DeclarationStatus = "declared_only"

type Channel struct {
	Key         model.Hash      `json:"key"`
	Address     string          `json:"address,omitempty"`
	AddressKind AddressKind     `json:"address_kind"`
	JSONPointer string          `json:"json_pointer"`
	SourceRef   model.SourceRef `json:"source_ref"`
}

type Operation struct {
	Key         model.Hash      `json:"key"`
	Action      Action          `json:"action"`
	ChannelKey  model.Hash      `json:"channel_key"`
	MessageKeys []model.Hash    `json:"message_keys"`
	JSONPointer string          `json:"json_pointer"`
	SourceRef   model.SourceRef `json:"source_ref"`
}

type Message struct {
	Key         model.Hash      `json:"key"`
	ChannelKey  model.Hash      `json:"channel_key"`
	Name        string          `json:"name,omitempty"`
	JSONPointer string          `json:"json_pointer"`
	SourceRef   model.SourceRef `json:"source_ref"`
}

type PayloadField struct {
	Key          model.Hash      `json:"key"`
	MessageKey   model.Hash      `json:"message_key"`
	Name         string          `json:"name"`
	Types        []string        `json:"types"`
	Required     bool            `json:"required"`
	Nullable     bool            `json:"nullable"`
	Format       string          `json:"format,omitempty"`
	DocumentPath string          `json:"document_path"`
	JSONPointer  string          `json:"json_pointer"`
	SourceRef    model.SourceRef `json:"source_ref"`
}

type Reference struct {
	FromPath    string           `json:"from_path"`
	FromPointer string           `json:"from_pointer"`
	TargetPath  string           `json:"target_path,omitempty"`
	Target      string           `json:"target_pointer,omitempty"`
	Status      schema.RefStatus `json:"status"`
}

type DeliveryControl struct {
	Kind         string            `json:"kind"`
	FieldPath    string            `json:"field_path"`
	Status       DeclarationStatus `json:"status"`
	DocumentPath string            `json:"document_path"`
	JSONPointer  string            `json:"json_pointer"`
	SourceRef    model.SourceRef   `json:"source_ref"`
}

type Result struct {
	FormatVersion   uint16              `json:"format_version"`
	Identity        schema.Identity     `json:"identity"`
	IdentityKey     model.Hash          `json:"identity_key"`
	AsyncAPIVersion string              `json:"asyncapi_version,omitempty"`
	Coverage        schema.Coverage     `json:"coverage"`
	Channels        []Channel           `json:"channels"`
	Operations      []Operation         `json:"operations"`
	Messages        []Message           `json:"messages"`
	PayloadFields   []PayloadField      `json:"payload_fields"`
	Controls        []DeliveryControl   `json:"delivery_controls"`
	References      []Reference         `json:"references"`
	Diagnostics     []schema.Diagnostic `json:"diagnostics"`
}

type Input struct {
	Identity schema.Identity
	Bundle   schema.Bundle
}

type analyzer struct {
	result      *Result
	identityKey model.Hash
	rootPath    string
	docs        map[string]schema.SourceDocument
	values      map[string]any
	channels    map[string]Channel
	channelMsgs map[string]map[string]bool
	messageKeys map[string]model.Hash
	active      map[string]bool
	seenRefs    map[string]bool
	seenDiag    map[string]bool
	seenControl map[string]bool
}

// Analyze accepts only a verified in-memory source bundle and the explicit
// AsyncAPI 3.1.0 JSON subset described by the product contract.
func Analyze(input Input) (Result, error) {
	result := Result{
		FormatVersion: 1, Identity: input.Identity, Coverage: schema.CoverageComplete,
		Channels: []Channel{}, Operations: []Operation{}, Messages: []Message{},
		PayloadFields: []PayloadField{}, Controls: []DeliveryControl{},
		References: []Reference{}, Diagnostics: []schema.Diagnostic{},
	}
	if input.Identity.Protocol != schema.ProtocolAsyncAPI31 {
		return Result{}, fmt.Errorf("invalid event contract: protocol profile must be %q", schema.ProtocolAsyncAPI31)
	}
	docs, err := input.Bundle.Validate(input.Identity)
	if err != nil {
		return Result{}, err
	}
	identityKey, err := input.Identity.Key()
	if err != nil {
		return Result{}, err
	}
	values, err := schema.DecodeJSONBundle(docs)
	if err != nil {
		return Result{}, fmt.Errorf("invalid AsyncAPI bundle: %v", err)
	}
	root, ok := values[input.Bundle.RootPath].(map[string]any)
	if !ok {
		return Result{}, fmt.Errorf("invalid AsyncAPI bundle: root must be an object")
	}
	result.IdentityKey = identityKey
	result.AsyncAPIVersion, _ = root["asyncapi"].(string)
	a := &analyzer{
		result: &result, identityKey: identityKey, rootPath: input.Bundle.RootPath,
		docs: docs, values: values, channels: map[string]Channel{},
		channelMsgs: map[string]map[string]bool{}, messageKeys: map[string]model.Hash{},
		active: map[string]bool{}, seenRefs: map[string]bool{},
		seenDiag: map[string]bool{}, seenControl: map[string]bool{},
	}
	if result.AsyncAPIVersion != AsyncAPI31 {
		a.addDiagnostic("unsupported_asyncapi_version", "/asyncapi", "only AsyncAPI 3.1.0 is supported; declarations were not interpreted")
	} else {
		a.analyzeRoot(root)
	}
	if len(result.Diagnostics) > 0 {
		result.Coverage = schema.CoveragePartial
	}
	a.sortResult()
	return result, nil
}

func (a *analyzer) analyzeRoot(root map[string]any) {
	if info, ok := root["info"].(map[string]any); !ok {
		a.addDiagnostic("invalid_info", "/info", "AsyncAPI info must be an object")
	} else if version, ok := info["version"].(string); !ok || version != string(a.result.Identity.Version) {
		a.addDiagnostic("contract_version_mismatch", "/info/version", "contract identity version must match the declared AsyncAPI version")
	}
	channels, ok := root["channels"].(map[string]any)
	if !ok {
		a.addDiagnostic("invalid_channels", "/channels", "AsyncAPI channels must be an object")
	} else {
		a.readChannels(channels)
	}
	if operations, ok := root["operations"].(map[string]any); !ok {
		a.addDiagnostic("invalid_operations", "/operations", "AsyncAPI operations must be an object")
	} else {
		a.readOperations(operations)
	}
}

func (a *analyzer) readChannels(raw map[string]any) {
	for _, name := range sortedKeys(raw) {
		pointer := eventPointer("/channels", name)
		value, ok := raw[name].(map[string]any)
		if !ok {
			a.addDiagnostic("invalid_channel", pointer, "channel declaration must be an object")
			continue
		}
		if _, isReferenceObject := value["$ref"]; isReferenceObject {
			a.addDiagnostic("unsupported_channel_reference_object", pointer, "channel declarations must be inline objects in this subset")
			continue
		}
		address, _ := value["address"].(string)
		if rawAddress, exists := value["address"]; exists {
			if _, ok := rawAddress.(string); !ok {
				a.addDiagnostic("invalid_channel_address", eventPointer(pointer, "address"), "channel address must be a string when present")
			}
		}
		addressKind := AddressLiteral
		if address == "" {
			addressKind = AddressUnknown
			address = ""
		} else if !safeAddress(address) {
			addressKind = AddressUnknown
			address = ""
			a.addDiagnostic("unsafe_channel_address", eventPointer(pointer, "address"), "channel address is not exposed because it is outside the safe bounded subset")
		} else if strings.ContainsAny(address, "{}*#+") {
			addressKind = AddressTemplate
		}
		key, err := model.HashValue("event-channel-v1", map[string]any{"identity_key": a.identityKey, "pointer": pointer})
		if err != nil {
			a.addDiagnostic("invalid_identity", pointer, "channel identity could not be computed")
			continue
		}
		channel := Channel{Key: key, Address: address, AddressKind: addressKind, JSONPointer: pointer, SourceRef: a.docs[a.rootPath].Ref}
		channelID := a.rootPath + "#" + pointer
		a.channels[channelID] = channel
		a.channelMsgs[channelID] = map[string]bool{}
		if messages, ok := value["messages"].(map[string]any); ok {
			for _, messageName := range sortedKeys(messages) {
				messagePointer := eventPointer(eventPointer(pointer, "messages"), messageName)
				if message, found := a.resolveObject(a.rootPath, messagePointer, messages[messageName]); found {
					a.addMessage(key, messageName, message.path, message.pointer, message.object)
					if message.path != "" && message.pointer != "" {
						a.channelMsgs[channelID][message.path+"#"+message.pointer] = true
					}
				}
			}
		} else if _, exists := value["messages"]; exists {
			a.addDiagnostic("invalid_channel_messages", eventPointer(pointer, "messages"), "channel messages must be an object")
		}
		a.addControls(a.rootPath, pointer, value["bindings"])
		a.result.Channels = append(a.result.Channels, channel)
	}
}

func (a *analyzer) readOperations(raw map[string]any) {
	for _, name := range sortedKeys(raw) {
		pointer := eventPointer("/operations", name)
		value, ok := raw[name].(map[string]any)
		if !ok {
			a.addDiagnostic("invalid_operation", pointer, "operation declaration must be an object")
			continue
		}
		action, _ := value["action"].(string)
		if action != string(ActionSend) && action != string(ActionReceive) {
			a.addDiagnostic("unsupported_operation_action", eventPointer(pointer, "action"), "operation action must be send or receive under AsyncAPI 3.1.0")
			continue
		}
		channelValue, exists := value["channel"]
		channelRef, ok := channelValue.(map[string]any)
		ref, isRef := "", false
		if ok {
			ref, isRef = channelRef["$ref"].(string)
		}
		if !exists || !isRef {
			a.addDiagnostic("invalid_operation_channel", eventPointer(pointer, "channel"), "operation channel must be a source-bound reference object")
			continue
		}
		resolved, channel, good := a.resolveChannel(pointer, ref)
		if !good {
			continue
		}
		operationKey, err := model.HashValue("event-operation-v1", map[string]any{
			"identity_key": a.identityKey, "pointer": pointer, "action": action, "channel_key": channel.Key,
		})
		if err != nil {
			a.addDiagnostic("invalid_identity", pointer, "operation identity could not be computed")
			continue
		}
		operation := Operation{Key: operationKey, Action: Action(action), ChannelKey: channel.Key, MessageKeys: []model.Hash{}, JSONPointer: pointer, SourceRef: a.docs[a.rootPath].Ref}
		messageValues, exists := value["messages"].([]any)
		if !exists {
			a.addDiagnostic("invalid_operation_messages", eventPointer(pointer, "messages"), "operation messages must be an array of source-bound references")
		} else {
			seenMessageKeys := map[model.Hash]bool{}
			for index, messageValue := range messageValues {
				messagePointer := eventPointer(eventPointer(pointer, "messages"), fmt.Sprint(index))
				message, found := a.resolveObject(a.rootPath, messagePointer, messageValue)
				if !found {
					continue
				}
				sourceKey := message.path + "#" + message.pointer
				if !a.channelMsgs[resolved][sourceKey] {
					a.addDiagnostic("operation_message_not_on_channel", messagePointer, "operation message is not declared by its referenced channel")
					continue
				}
				if key, exists := a.messageKeys[string(channel.Key)+"#"+sourceKey]; exists {
					if seenMessageKeys[key] {
						a.addDiagnostic("duplicate_operation_message", messagePointer, "operation message references must be unique")
						continue
					}
					seenMessageKeys[key] = true
					operation.MessageKeys = append(operation.MessageKeys, key)
				}
			}
		}
		a.addControls(a.rootPath, pointer, value["bindings"])
		a.result.Operations = append(a.result.Operations, operation)
	}
}

type resolvedObject struct {
	path    string
	pointer string
	object  map[string]any
}

func (a *analyzer) resolveChannel(fromPointer, reference string) (string, Channel, bool) {
	resolved := schema.ResolveBundleRef(a.values, a.rootPath, reference)
	referencePointer := eventPointer(eventPointer(fromPointer, "channel"), "$ref")
	a.addReference(a.rootPath, referencePointer, resolved)
	if resolved.Status != schema.RefResolved {
		a.addDiagnostic(refCode(resolved.Status), referencePointer, "channel reference is unresolved within the supplied source bundle")
		return "", Channel{}, false
	}
	channel, ok := a.channels[resolved.Path+"#"+resolved.Pointer]
	if !ok {
		a.addDiagnostic("unsupported_channel_reference", referencePointer, "channel reference does not identify a declared AsyncAPI channel")
		return "", Channel{}, false
	}
	return resolved.Path + "#" + resolved.Pointer, channel, true
}

func (a *analyzer) resolveObject(fromPath, pointer string, raw any) (resolvedObject, bool) {
	return a.resolveObjectDepth(fromPath, pointer, raw, 0)
}

func (a *analyzer) resolveObjectDepth(fromPath, pointer string, raw any, depth int) (resolvedObject, bool) {
	if depth > schema.MaxRefDepth {
		a.addDiagnosticAt(fromPath, "reference_depth_limit", pointer, "reference expansion exceeds the supported bound")
		return resolvedObject{}, false
	}
	object, ok := raw.(map[string]any)
	if !ok {
		a.addDiagnosticAt(fromPath, "invalid_reference_object", pointer, "declaration must be an object or a local reference object")
		return resolvedObject{}, false
	}
	if rawRef, exists := object["$ref"]; exists {
		reference, ok := rawRef.(string)
		if !ok || reference == "" {
			a.addDiagnosticAt(fromPath, "invalid_reference", eventPointer(pointer, "$ref"), "reference must be a nonempty source-relative URI")
			return resolvedObject{}, false
		}
		resolved := schema.ResolveBundleRef(a.values, fromPath, reference)
		targetID := resolved.Path + "#" + resolved.Pointer
		if resolved.Status == schema.RefResolved && a.active[targetID] {
			resolved.Status = schema.RefCycle
		}
		a.addReference(fromPath, eventPointer(pointer, "$ref"), resolved)
		if resolved.Status != schema.RefResolved {
			a.addDiagnosticAt(fromPath, refCode(resolved.Status), eventPointer(pointer, "$ref"), "reference is unresolved within the supplied source bundle")
			return resolvedObject{}, false
		}
		target, ok := resolved.Target.(map[string]any)
		if !ok {
			a.addDiagnosticAt(fromPath, "invalid_reference_target", eventPointer(pointer, "$ref"), "reference target must be an object")
			return resolvedObject{}, false
		}
		a.active[targetID] = true
		defer delete(a.active, targetID)
		return a.resolveObjectDepth(resolved.Path, resolved.Pointer, target, depth+1)
	}
	return resolvedObject{path: fromPath, pointer: pointer, object: object}, true
}

func (a *analyzer) addMessage(channelKey model.Hash, name, documentPath, pointer string, value map[string]any) {
	if documentPath == "" || pointer == "" {
		return
	}
	messageKey, err := model.HashValue("event-message-v1", map[string]any{
		"identity_key": a.identityKey, "channel_key": channelKey,
		"document": documentPath, "pointer": pointer,
	})
	if err != nil {
		a.addDiagnosticAt(documentPath, "invalid_identity", pointer, "message identity could not be computed")
		return
	}
	messageID := string(channelKey) + "#" + documentPath + "#" + pointer
	if _, exists := a.messageKeys[messageID]; exists {
		return
	}
	a.messageKeys[messageID] = messageKey
	a.result.Messages = append(a.result.Messages, Message{
		Key: messageKey, ChannelKey: channelKey, Name: name, JSONPointer: pointer,
		SourceRef: a.docs[documentPath].Ref,
	})
	a.addControls(documentPath, pointer, value["bindings"])
	a.readPayload(messageKey, documentPath, pointer, value)
}

func (a *analyzer) readPayload(messageKey model.Hash, documentPath, messagePointer string, message map[string]any) {
	rawPayload, exists := message["payload"]
	if !exists {
		return
	}
	format, _ := message["schemaFormat"].(string)
	if format == "" {
		format = DefaultSchemaFormat
	}
	if format != DefaultSchemaFormat && format != JSONSchemaDraft07Format {
		a.addDiagnosticAt(documentPath, "unsupported_schema_format", eventPointer(messagePointer, "schemaFormat"), "payload schema format is outside the supported AsyncAPI 3.1.0 subset")
		return
	}
	payloadPointer := eventPointer(messagePointer, "payload")
	if schemaObject, ok := rawPayload.(map[string]any); !ok {
		a.addDiagnosticAt(documentPath, "invalid_payload_schema", payloadPointer, "payload schema must be an object")
	} else {
		a.walkPayload(messageKey, documentPath, payloadPointer, schemaObject, 0)
	}
}

func (a *analyzer) walkPayload(messageKey model.Hash, documentPath, pointer string, node map[string]any, depth int) {
	if depth > schema.MaxRefDepth {
		a.addDiagnosticAt(documentPath, "reference_depth_limit", pointer, "payload reference expansion exceeds the supported bound")
		return
	}
	ref, hasRef := node["$ref"].(string)
	if _, exists := node["$ref"]; exists && (!hasRef || ref == "") {
		a.addDiagnosticAt(documentPath, "invalid_reference", eventPointer(pointer, "$ref"), "payload reference must be a nonempty source-relative URI")
		return
	}
	if hasRef {
		resolved := schema.ResolveBundleRef(a.values, documentPath, ref)
		referencePointer := eventPointer(pointer, "$ref")
		if resolved.Status == schema.RefResolved {
			targetID := resolved.Path + "#" + resolved.Pointer
			if a.active[targetID] {
				resolved.Status = schema.RefCycle
			}
		}
		a.addReference(documentPath, referencePointer, resolved)
		if resolved.Status != schema.RefResolved {
			a.addDiagnosticAt(documentPath, refCode(resolved.Status), referencePointer, "payload reference is unresolved within the supplied source bundle")
			return
		}
		target, ok := resolved.Target.(map[string]any)
		if !ok {
			a.addDiagnosticAt(documentPath, "invalid_payload_schema", referencePointer, "payload reference target must be a schema object")
			return
		}
		targetID := resolved.Path + "#" + resolved.Pointer
		a.active[targetID] = true
		a.walkPayload(messageKey, resolved.Path, resolved.Pointer, target, depth+1)
		delete(a.active, targetID)
		return
	}
	if len(a.result.PayloadFields) >= schema.MaxFacts {
		a.addDiagnosticAt(documentPath, "fact_limit", pointer, "event payload field count exceeds the supported bound")
		return
	}
	if properties, exists := node["properties"]; exists {
		propertyMap, ok := properties.(map[string]any)
		if !ok {
			a.addDiagnosticAt(documentPath, "invalid_payload_properties", eventPointer(pointer, "properties"), "payload properties must be an object")
		} else {
			requiredNames := map[string]bool{}
			if rawRequired, exists := node["required"]; exists {
				items, ok := rawRequired.([]any)
				if !ok {
					a.addDiagnosticAt(documentPath, "invalid_payload_required", eventPointer(pointer, "required"), "payload required must be an array of unique strings")
				} else {
					for _, item := range items {
						name, ok := item.(string)
						if !ok || name == "" || requiredNames[name] {
							a.addDiagnosticAt(documentPath, "invalid_payload_required", eventPointer(pointer, "required"), "payload required must contain unique nonempty names")
							continue
						}
						requiredNames[name] = true
					}
				}
			}
			for _, name := range sortedKeys(propertyMap) {
				propertyPointer := eventPointer(eventPointer(pointer, "properties"), name)
				declaration, ok := propertyMap[name].(map[string]any)
				if !ok {
					a.addDiagnosticAt(documentPath, "invalid_payload_field", propertyPointer, "payload field schema must be an object")
					continue
				}
				a.addPayloadField(messageKey, documentPath, propertyPointer, name, declaration, requiredNames[name])
				a.walkPayload(messageKey, documentPath, propertyPointer, declaration, depth)
			}
		}
	}
	if items, exists := node["items"]; exists {
		if itemSchema, ok := items.(map[string]any); ok {
			a.walkPayload(messageKey, documentPath, eventPointer(pointer, "items"), itemSchema, depth)
		} else {
			a.addDiagnosticAt(documentPath, "unsupported_payload_schema", eventPointer(pointer, "items"), "payload items must be a schema object")
		}
	}
	for _, key := range sortedKeys(node) {
		if payloadKeywordSupported(key) {
			continue
		}
		a.addDiagnosticAt(documentPath, "unsupported_payload_keyword", eventPointer(pointer, key), "payload keyword is not interpreted by the supported subset")
	}
}

func (a *analyzer) addPayloadField(messageKey model.Hash, documentPath, pointer, name string, declaration map[string]any, required bool) {
	types := []string{}
	nullable := false
	switch rawType := declaration["type"].(type) {
	case string:
		types = append(types, rawType)
		nullable = rawType == "null"
	case []any:
		seen := map[string]bool{}
		for _, raw := range rawType {
			typeName, ok := raw.(string)
			if !ok || seen[typeName] {
				a.addDiagnosticAt(documentPath, "invalid_payload_type", eventPointer(pointer, "type"), "payload type array must contain unique strings")
				continue
			}
			seen[typeName] = true
			types = append(types, typeName)
			nullable = nullable || typeName == "null"
		}
	case nil:
	default:
		a.addDiagnosticAt(documentPath, "invalid_payload_type", eventPointer(pointer, "type"), "payload type must be a string or string array")
	}
	sort.Strings(types)
	for _, typeName := range types {
		if typeName != "null" && typeName != "boolean" && typeName != "object" && typeName != "array" && typeName != "number" && typeName != "integer" && typeName != "string" {
			a.addDiagnosticAt(documentPath, "unsupported_payload_type", eventPointer(pointer, "type"), "payload type is outside the JSON Schema vocabulary")
		}
	}
	if declaration["nullable"] == true {
		nullable = true
	}
	format := ""
	if rawFormat, exists := declaration["format"]; exists {
		if value, ok := rawFormat.(string); ok && safeFormat(value) {
			format = value
		} else {
			a.addDiagnosticAt(documentPath, "unsupported_payload_format", eventPointer(pointer, "format"), "payload format is not in the bounded allowlist")
		}
	}
	key, err := model.HashValue("event-payload-field-v1", map[string]any{
		"identity_key": a.identityKey, "message_key": messageKey, "document": documentPath, "pointer": pointer,
	})
	if err != nil {
		a.addDiagnosticAt(documentPath, "invalid_identity", pointer, "payload field identity could not be computed")
		return
	}
	a.result.PayloadFields = append(a.result.PayloadFields, PayloadField{
		Key: key, MessageKey: messageKey, Name: name, Types: types, Required: required,
		Nullable: nullable, Format: format, DocumentPath: documentPath, JSONPointer: pointer,
		SourceRef: a.docs[documentPath].Ref,
	})
}

func (a *analyzer) addControls(documentPath, pointer string, raw any) {
	if raw == nil {
		return
	}
	bindings, ok := raw.(map[string]any)
	if !ok {
		a.addDiagnosticAt(documentPath, "invalid_bindings", eventPointer(pointer, "bindings"), "protocol bindings must be an object")
		return
	}
	allowlist := map[string]map[string]string{
		"amqp":  {"ack": "ack", "deliveryMode": "delivery_mode", "mandatory": "mandatory"},
		"mqtt":  {"qos": "qos", "retain": "retain"},
		"mqtt5": {"qos": "qos", "retain": "retain"},
		"kafka": {"groupId": "group_id", "clientId": "client_id"},
	}
	for _, protocol := range sortedKeys(bindings) {
		fields, ok := bindings[protocol].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range sortedKeys(fields) {
			kind, allowed := allowlist[protocol][field]
			if !allowed {
				continue
			}
			fieldPath := protocol + "." + field
			sourcePointer := eventPointer(eventPointer(eventPointer(pointer, "bindings"), protocol), field)
			controlID := documentPath + "#" + sourcePointer
			if a.seenControl[controlID] {
				continue
			}
			a.seenControl[controlID] = true
			a.result.Controls = append(a.result.Controls, DeliveryControl{
				Kind: kind, FieldPath: fieldPath, Status: BindingDeclaredOnly,
				DocumentPath: documentPath, JSONPointer: sourcePointer, SourceRef: a.docs[documentPath].Ref,
			})
		}
	}
}

func (a *analyzer) addReference(fromPath, fromPointer string, resolved schema.ResolvedRef) {
	id := fromPath + "#" + fromPointer + "#" + string(resolved.Status) + "#" + resolved.Path + "#" + resolved.Pointer
	if a.seenRefs[id] {
		return
	}
	a.seenRefs[id] = true
	a.result.References = append(a.result.References, Reference{
		FromPath: fromPath, FromPointer: fromPointer, TargetPath: resolved.Path,
		Target: resolved.Pointer, Status: resolved.Status,
	})
}

func (a *analyzer) addDiagnostic(code, pointer, detail string) {
	a.addDiagnosticAt(a.rootPath, code, pointer, detail)
}

func (a *analyzer) addDiagnosticAt(documentPath, code, pointer, detail string) {
	id := documentPath + "#" + pointer + "#" + code
	if a.seenDiag[id] {
		return
	}
	a.seenDiag[id] = true
	a.result.Diagnostics = append(a.result.Diagnostics, schema.Diagnostic{
		Code: code, DocumentPath: documentPath, Pointer: pointer, Detail: detail,
	})
}

func (a *analyzer) sortResult() {
	sort.Slice(a.result.Channels, func(i, j int) bool { return a.result.Channels[i].JSONPointer < a.result.Channels[j].JSONPointer })
	sort.Slice(a.result.Operations, func(i, j int) bool { return a.result.Operations[i].JSONPointer < a.result.Operations[j].JSONPointer })
	sort.Slice(a.result.Messages, func(i, j int) bool {
		left, right := a.result.Messages[i], a.result.Messages[j]
		if left.ChannelKey != right.ChannelKey {
			return left.ChannelKey < right.ChannelKey
		}
		return left.JSONPointer < right.JSONPointer
	})
	sort.Slice(a.result.PayloadFields, func(i, j int) bool {
		left, right := a.result.PayloadFields[i], a.result.PayloadFields[j]
		if left.DocumentPath != right.DocumentPath {
			return left.DocumentPath < right.DocumentPath
		}
		return left.JSONPointer < right.JSONPointer
	})
	sort.Slice(a.result.Controls, func(i, j int) bool {
		left, right := a.result.Controls[i], a.result.Controls[j]
		if left.DocumentPath != right.DocumentPath {
			return left.DocumentPath < right.DocumentPath
		}
		return left.JSONPointer < right.JSONPointer
	})
	sort.Slice(a.result.References, func(i, j int) bool {
		left, right := a.result.References[i], a.result.References[j]
		if left.FromPath != right.FromPath {
			return left.FromPath < right.FromPath
		}
		return left.FromPointer < right.FromPointer
	})
	sort.Slice(a.result.Diagnostics, func(i, j int) bool {
		left, right := a.result.Diagnostics[i], a.result.Diagnostics[j]
		if left.DocumentPath != right.DocumentPath {
			return left.DocumentPath < right.DocumentPath
		}
		if left.Pointer != right.Pointer {
			return left.Pointer < right.Pointer
		}
		return left.Code < right.Code
	})
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func eventPointer(pointer, token string) string {
	token = strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
	return pointer + "/" + token
}

func safeAddress(address string) bool {
	return len(address) <= maxAddressBytes && utf8.ValidString(address) &&
		!strings.ContainsAny(address, "\x00\r\n\t ?%") && !strings.Contains(address, "://") && !strings.Contains(address, "@")
}

func safeFormat(value string) bool {
	switch value {
	case "date-time", "date", "time", "email", "hostname", "ipv4", "ipv6", "uri", "uuid", "regex", "byte", "binary", "duration":
		return true
	default:
		return false
	}
}

func payloadKeywordSupported(key string) bool {
	switch key {
	case "$ref", "$schema", "$id", "$comment", "title", "description", "type", "format", "nullable", "properties", "required", "items", "additionalProperties", "readOnly", "writeOnly", "deprecated", "examples", "default", "enum", "const", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "pattern", "minItems", "maxItems", "uniqueItems", "minProperties", "maxProperties", "contentEncoding", "contentMediaType":
		return true
	default:
		return false
	}
}

func refCode(status schema.RefStatus) string {
	switch status {
	case schema.RefRemote:
		return "remote_reference_unresolved"
	case schema.RefTraversal:
		return "unsafe_reference_unresolved"
	case schema.RefMissing:
		return "missing_reference_unresolved"
	case schema.RefCycle:
		return "cyclic_reference_unresolved"
	default:
		return "unsupported_reference_unresolved"
	}
}
