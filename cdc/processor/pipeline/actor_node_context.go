// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pipeline

import (
	sdtContext "context"
	"sync/atomic"

	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/pkg/actor"
	"github.com/pingcap/tiflow/pkg/actor/message"
	"github.com/pingcap/tiflow/pkg/context"
	"github.com/pingcap/tiflow/pkg/pipeline"
	"go.uber.org/zap"
)

// send a tick message to actor if we get 32 pipeline messages
const messagesPerTick = 32

// actorNodeContext implements the NodeContext interface, with this we do not need
// to change too much logic to implement the table actor.
// the SendToNextNode buffer the pipeline message and tick the actor system
// the Throw function handle error and stop the actor
type actorNodeContext struct {
	sdtContext.Context
	outputCh             chan pipeline.Message
	tableActorRouter     *actor.Router
	tableActorID         actor.ID
	changefeedVars       *context.ChangefeedVars
	globalVars           *context.GlobalVars
	tickMessageThreshold int32
	// noTickMessageCount is the count of pipeline message that no tick message is sent to actor
	noTickMessageCount int32
	tableName          string
	throw              func(error)
}

func newContext(stdCtx sdtContext.Context,
	tableName string,
	tableActorRouter *actor.Router,
	tableActorID actor.ID,
	changefeedVars *context.ChangefeedVars,
	globalVars *context.GlobalVars,
	throw func(error)) *actorNodeContext {
	return &actorNodeContext{
		Context:              stdCtx,
		outputCh:             make(chan pipeline.Message, defaultOutputChannelSize),
		tableActorRouter:     tableActorRouter,
		tableActorID:         tableActorID,
		changefeedVars:       changefeedVars,
		globalVars:           globalVars,
		tickMessageThreshold: messagesPerTick,
		noTickMessageCount:   0,
		tableName:            tableName,
		throw:                throw,
	}
}

func (c *actorNodeContext) setTickMessageThreshold(threshold int32) {
	atomic.StoreInt32(&c.tickMessageThreshold, threshold)
}

func (c *actorNodeContext) GlobalVars() *context.GlobalVars {
	return c.globalVars
}

func (c *actorNodeContext) ChangefeedVars() *context.ChangefeedVars {
	return c.changefeedVars
}

func (c *actorNodeContext) Throw(err error) {
	// node error will be reported to processor, and then processor cancel table
	c.throw(err)
}

// SendToNextNode send msg to the outputCh and notify the actor system,
// to reduce the  actor message, only send tick message per threshold
func (c *actorNodeContext) SendToNextNode(msg pipeline.Message) {
	select {
	// if the processor context is cancelled, return directly
	// otherwise processor tick loop will be blocked if the chan is full， because actor is topped
	case <-c.Context.Done():
		log.Info("context is canceled",
			zap.String("tableName", c.tableName),
			zap.String("changefeed", c.changefeedVars.ID))
	case c.outputCh <- msg:
		c.trySendTickMessage()
	}
}

func (c *actorNodeContext) TrySendToNextNode(msg pipeline.Message) bool {
	added := false
	select {
	case c.outputCh <- msg:
		added = true
	default:
	}
	if added {
		c.trySendTickMessage()
	}
	return added
}

func (c *actorNodeContext) Message() pipeline.Message {
	return <-c.outputCh
}

func (c *actorNodeContext) tryGetProcessedMessage() *pipeline.Message {
	select {
	case msg, ok := <-c.outputCh:
		if !ok {
			return nil
		}
		return &msg
	default:
		return nil
	}
}

func (c *actorNodeContext) trySendTickMessage() {
	threshold := atomic.LoadInt32(&c.tickMessageThreshold)
	atomic.AddInt32(&c.noTickMessageCount, 1)
	count := atomic.LoadInt32(&c.noTickMessageCount)
	// resolvedTs message will be sent by puller periodically
	if count >= threshold {
		_ = c.tableActorRouter.Send(c.tableActorID, message.TickMessage())
		atomic.StoreInt32(&c.noTickMessageCount, 0)
	}
}
