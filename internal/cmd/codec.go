package cmd

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// registry lists every command type by name. The name is what's written in
// the log, so renaming a command type breaks replaying old logs: add a new
// name instead, and keep the old one decoding.
var registry = map[string]func() Command{
	"SetPrimaryDomain":   func() Command { return new(SetPrimaryDomain) },
	"AddAlternateDomain": func() Command { return new(AddAlternateDomain) },
	"AddHostAlias":       func() Command { return new(AddHostAlias) },
	"CreateGroup":        func() Command { return new(CreateGroup) },
	"CreateLogin":        func() Command { return new(CreateLogin) },
	"FailLoginCode":      func() Command { return new(FailLoginCode) },
	"RedeemLogin":        func() Command { return new(RedeemLogin) },
	"SetHandle":          func() Command { return new(SetHandle) },
	"EndSession":         func() Command { return new(EndSession) },
	"PutCert":            func() Command { return new(PutCert) },
	"DeleteCert":         func() Command { return new(DeleteCert) },
	"Purge":              func() Command { return new(Purge) },

	// M1: posts and discussion
	"JoinGroup":     func() Command { return new(JoinGroup) },
	"CreatePost":    func() Command { return new(CreatePost) },
	"EditPost":      func() Command { return new(EditPost) },
	"CreateComment": func() Command { return new(CreateComment) },
	"EditComment":   func() Command { return new(EditComment) },
	"SoftDelete":    func() Command { return new(SoftDelete) },
	"Restore":       func() Command { return new(Restore) },
	"ImportPost":    func() Command { return new(ImportPost) },

	// M2: links, digests, the job queue, settings, usage
	"ClaimJob":       func() Command { return new(ClaimJob) },
	"FailJob":        func() Command { return new(FailJob) },
	"AddLink":        func() Command { return new(AddLink) },
	"RemoveLink":     func() Command { return new(RemoveLink) },
	"SetCheck":       func() Command { return new(SetCheck) },
	"SetNote":        func() Command { return new(SetNote) },
	"SetDigest":      func() Command { return new(SetDigest) },
	"MoveUnder":      func() Command { return new(MoveUnder) },
	"MoveOut":        func() Command { return new(MoveOut) },
	"UpdateSettings": func() Command { return new(UpdateSettings) },
	"RecordUsage":    func() Command { return new(RecordUsage) },
	"SaveFeedback":   func() Command { return new(SaveFeedback) },
}

// envelope is how a command sits in the log: its type name plus its fields
// as JSON. JSON because it's readable when debugging a log, and the
// commands are small.
type envelope struct {
	Type string          `json:"t"`
	Cmd  json.RawMessage `json:"c"`
}

func nameOf(c Command) string {
	t := reflect.TypeOf(c)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}

// Encode turns a command into bytes for the log.
func Encode(c Command) ([]byte, error) {
	name := nameOf(c)
	if _, ok := registry[name]; !ok {
		return nil, fmt.Errorf("command %s is not in the registry", name)
	}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{Type: name, Cmd: body})
}

// Decode turns log bytes back into a command.
func Decode(data []byte) (Command, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	mk, ok := registry[env.Type]
	if !ok {
		return nil, fmt.Errorf("unknown command %q", env.Type)
	}
	c := mk()
	if err := json.Unmarshal(env.Cmd, c); err != nil {
		return nil, fmt.Errorf("%s: %w", env.Type, err)
	}
	return c, nil
}
