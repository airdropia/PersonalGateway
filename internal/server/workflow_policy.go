package server

import (
	"context"

	"github.com/airdropia/pgw/internal/core"
	"github.com/airdropia/pgw/internal/gateway"
)

// RequestWorkflowPolicyResolver matches persisted workflow versions for requests.
type RequestWorkflowPolicyResolver = gateway.WorkflowPolicyResolver

func applyWorkflowPolicy(ctx context.Context, workflow *core.Workflow, resolver RequestWorkflowPolicyResolver, selector core.WorkflowSelector) error {
	return gateway.ApplyWorkflowPolicy(ctx, workflow, resolver, selector)
}
