package codexappserver

import (
	"context"
	"errors"
	"fmt"
)

func (c *Client) ListLoadedThreads(ctx context.Context, params ThreadLoadedListParams) (ThreadLoadedListResult, error) {
	var result ThreadLoadedListResult
	if err := c.Call(ctx, MethodThreadLoadedList, params, &result); err != nil {
		return ThreadLoadedListResult{}, err
	}
	if result.Data == nil {
		return ThreadLoadedListResult{}, fmt.Errorf("%w: thread/loaded/list result is missing data", ErrProtocol)
	}
	return result, nil
}

func (c *Client) ReadThread(ctx context.Context, params ThreadReadParams) (Thread, error) {
	if params.ThreadID == "" {
		return Thread{}, errors.New("codexappserver: thread/read needs threadId")
	}
	var result ThreadReadResult
	if err := c.Call(ctx, MethodThreadRead, params, &result); err != nil {
		return Thread{}, err
	}
	if err := validateThread(result.Thread); err != nil {
		return Thread{}, err
	}
	return result.Thread, nil
}

func validateThread(thread Thread) error {
	if thread.ID == "" || len(thread.Status) == 0 || thread.Turns == nil {
		return fmt.Errorf("%w: thread result is missing id, status, or turns", ErrProtocol)
	}
	for _, turn := range thread.Turns {
		if err := validateTurn(turn); err != nil {
			return err
		}
	}
	return nil
}

func validateTurn(turn Turn) error {
	if turn.ID == "" || turn.Status == "" || turn.Items == nil {
		return fmt.Errorf("%w: turn result is missing id, status, or items", ErrProtocol)
	}
	return nil
}

func (c *Client) SetThreadName(ctx context.Context, threadID, name string) error {
	if threadID == "" || name == "" {
		return errors.New("codexappserver: thread/name/set needs threadId and name")
	}
	return c.Call(ctx, MethodThreadNameSet, ThreadNameSetParams{ThreadID: threadID, Name: name}, nil)
}

func (c *Client) StartCompaction(ctx context.Context, threadID string) error {
	if threadID == "" {
		return errors.New("codexappserver: thread/compact/start needs threadId")
	}
	return c.Call(ctx, MethodThreadCompactStart, ThreadCompactStartParams{ThreadID: threadID}, nil)
}

func (c *Client) StartTurn(ctx context.Context, params TurnStartParams) (Turn, error) {
	if params.ThreadID == "" || len(params.Input) == 0 {
		return Turn{}, errors.New("codexappserver: turn/start needs threadId and input")
	}
	var result TurnStartResult
	if err := c.Call(ctx, MethodTurnStart, params, &result); err != nil {
		return Turn{}, err
	}
	if err := validateTurn(result.Turn); err != nil {
		return Turn{}, err
	}
	return result.Turn, nil
}

func (c *Client) SteerTurn(ctx context.Context, params TurnSteerParams) (string, error) {
	if params.ThreadID == "" || params.ExpectedTurnID == "" || len(params.Input) == 0 {
		return "", errors.New("codexappserver: turn/steer needs threadId, expectedTurnId, and input")
	}
	var result TurnSteerResult
	if err := c.Call(ctx, MethodTurnSteer, params, &result); err != nil {
		return "", err
	}
	if result.TurnID == "" {
		return "", fmt.Errorf("%w: turn/steer result is missing turnId", ErrProtocol)
	}
	return result.TurnID, nil
}

func (c *Client) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	if threadID == "" || turnID == "" {
		return errors.New("codexappserver: turn/interrupt needs threadId and turnId")
	}
	return c.Call(ctx, MethodTurnInterrupt, TurnInterruptParams{ThreadID: threadID, TurnID: turnID}, nil)
}

func (c *Client) ReadAccountRateLimits(ctx context.Context) (AccountRateLimitsReadResult, error) {
	var result AccountRateLimitsReadResult
	if err := c.Call(ctx, MethodAccountRateLimitsRead, struct{}{}, &result); err != nil {
		return AccountRateLimitsReadResult{}, err
	}
	return result, nil
}
