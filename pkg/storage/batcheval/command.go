// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// A Command is the implementation of a single request within a BatchRequest.
type Command struct {
	// DeclareKeys adds all keys this command touches, and when (if applicable), to the given SpanSet.
	// TODO(nvanbenschoten): rationalize this RangeDescriptor. Can it change
	// between key declaration and cmd evaluation?
	DeclareKeys func(*roachpb.RangeDescriptor, roachpb.Header, roachpb.Request, *spanset.SpanSet)

	// Eval{RW,RO} evaluates a read-{write,only} command respectively on the
	// given engine.{ReadWriter,Reader}. It should populate the supplied
	// response (always a non-nil pointer to the correct type) and return
	// special side effects (if any) in the Result. If it writes to the engine
	// it should also update *CommandArgs.Stats. It should treat the provided
	// request as immutable.
	//
	// Only one of these is ever set at a time.
	EvalRW func(context.Context, engine.ReadWriter, CommandArgs, roachpb.Response) (result.Result, error)
	EvalRO func(context.Context, engine.Reader, CommandArgs, roachpb.Response) (result.Result, error)
}

var cmds = make(map[roachpb.Method]Command)

// RegisterReadWriteCommand makes a read-write command available for execution.
// It must only be called before any evaluation takes place.
func RegisterReadWriteCommand(
	method roachpb.Method,
	declare func(*roachpb.RangeDescriptor, roachpb.Header, roachpb.Request, *spanset.SpanSet),
	impl func(context.Context, engine.ReadWriter, CommandArgs, roachpb.Response) (result.Result, error),
) {
	register(method, Command{
		DeclareKeys: declare,
		EvalRW:      impl,
	})
}

// RegisterReadOnlyCommand makes a read-only command available for execution. It
// must only be called before any evaluation takes place.
func RegisterReadOnlyCommand(
	method roachpb.Method,
	declare func(*roachpb.RangeDescriptor, roachpb.Header, roachpb.Request, *spanset.SpanSet),
	impl func(context.Context, engine.Reader, CommandArgs, roachpb.Response) (result.Result, error),
) {
	register(method, Command{
		DeclareKeys: declare,
		EvalRO:      impl,
	})
}

func register(method roachpb.Method, command Command) {
	if _, ok := cmds[method]; ok {
		log.Fatalf(context.TODO(), "cannot overwrite previously registered method %v", method)
	}
	cmds[method] = command
}

// UnregisterCommand is provided for testing and allows removing a command.
// It is a no-op if the command is not registered.
func UnregisterCommand(method roachpb.Method) {
	delete(cmds, method)
}

// LookupCommand returns the command for the given method, with the boolean
// indicating success or failure.
func LookupCommand(method roachpb.Method) (Command, bool) {
	cmd, ok := cmds[method]
	return cmd, ok
}
