// Copyright 2020 New Relic Corporation. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package cmdchannel

import (
	"context"
	"time"

	"github.com/newrelic/infrastructure-agent/internal/feature_flags"

	"github.com/newrelic/infrastructure-agent/internal/agent/cmdchannel/handler"
	"github.com/newrelic/infrastructure-agent/internal/agent/id"
	"github.com/newrelic/infrastructure-agent/pkg/backend/commandapi"
	"github.com/newrelic/infrastructure-agent/pkg/config"
	"github.com/newrelic/infrastructure-agent/pkg/entity"
	"github.com/newrelic/infrastructure-agent/pkg/log"
)

var (
	ccsLogger = log.WithComponent("CommandChannelService")
)

// CmdHandle command channel request handler function.
type CmdHandle func(ctx context.Context, cmd commandapi.Command, initialFetch bool) (backoffSecs int, err error)

type srv struct {
	client            commandapi.Client
	pollDelaySecs     int
	handlersByCmdName map[string]CmdHandle
	ffHandler         *handler.FFHandler // explicit to ease deps injection on runtime
}

// NewService creates a service to poll and handle command channel commands.
func NewService(client commandapi.Client, config *config.Config, ffSetter feature_flags.Setter) Service {
	boHandle := func(ctx context.Context, cmd commandapi.Command, initialFetch bool) (backoffSecs int, err error) {
		boArgs, ok := cmd.Args.(commandapi.BackoffArgs)
		if !ok {
			err = handler.InvalidArgsErr
			return
		}
		backoffSecs = boArgs.Delay
		return
	}

	ffHandler := handler.NewFFHandler(config, ffSetter, log.WithComponent("FFHandler"))
	return &srv{
		client:        client,
		pollDelaySecs: config.CommandChannelIntervalSec,
		ffHandler:     ffHandler,
		handlersByCmdName: map[string]CmdHandle{
			commandapi.BackoffCmd: boHandle,
			commandapi.SetFFCmd:   ffHandler.Handle,
		},
	}
}

// InitialFetch initial poll to command channel
func (s *srv) InitialFetch(ctx context.Context) (InitialCmdResponse, error) {
	cmds, err := s.client.GetCommands(entity.EmptyID)
	if err != nil {
		return InitialCmdResponse{}, err
	}

	for _, cmd := range cmds {
		s.handle(ctx, cmd, true)
	}

	return InitialCmdResponse{
		Ts:    time.Now(),
		Delay: time.Duration(s.pollDelaySecs) * time.Second,
	}, nil
}

// Run polls command channel periodically, in case 1st poll returned a delay, it starts afterwards.
func (s *srv) Run(ctx context.Context, agentIDProvide id.Provide, initialRes InitialCmdResponse) {
	d := initialRes.Delay - time.Now().Sub(initialRes.Ts)
	if d <= 0 {
		d = s.nextPollInterval()
	}

	t := time.NewTicker(d)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cmds, err := s.client.GetCommands(agentIDProvide().ID)
			if err != nil {
				ccsLogger.WithError(err).Warn("commands poll failed")
			} else {
				for _, cmd := range cmds {
					s.handle(ctx, cmd, false)
				}
			}
			t.Stop()
			t = time.NewTicker(s.nextPollInterval())
		}
	}
}

// SetOHIHandler injects the handler dependency. A proper refactor of agent services injection will
// be required for this to be injected via srv constructor.
func (s *srv) SetOHIHandler(h handler.OHIEnabler) {
	s.ffHandler.SetOHIHandler(h)
}

func (s *srv) nextPollInterval() time.Duration {
	if s.pollDelaySecs <= 0 {
		s.pollDelaySecs = 1
	}
	return time.Duration(s.pollDelaySecs) * time.Second
}

func (s *srv) handle(ctx context.Context, c commandapi.Command, initialFetch bool) {
	handle, ok := s.handlersByCmdName[c.Name]
	if !ok {
		ccsLogger.
			WithField("cmd_id", c.ID).
			WithField("cmd_name", c.Name).
			Error("no handler for command-channel cmd")
		return
	}

	backoffSecs, err := handle(ctx, c, initialFetch)
	if err != nil {
		ccsLogger.
			WithField("cmd_id", c.ID).
			WithField("cmd_name", c.Name).
			WithField("cmd_arguments", c.Args).
			WithError(err).
			Error("error handling cmd-channel request")

	}
	if backoffSecs > 0 {
		s.pollDelaySecs = backoffSecs
	}
}
