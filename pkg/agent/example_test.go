package agent_test

import (
	"context"
	"fmt"
	"os"

	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/pkg/agent"
)

func ExampleRun() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := agent.Run(ctx, func(context.Context) (agent.Options, error) {
		return agent.Options{
			URL:   "wss://grayling.example.invalid",
			Token: os.Getenv("GRAYLING_AGENT_TOKEN"),
			Hello: &agentv1.Hello{AgentId: "checkout-agent", InstanceId: "checkout-1", BootId: "unique-process-boot-id"},
		}, nil
	}, func(client *agent.Client) error {
		if err := client.Subscribe(ctx, "checkout-policy", &agentv1.Cursor{Kind: agentv1.ResourceKind_RESOURCE_KIND_POLICY, Key: "checkout-policy"}); err != nil {
			return err
		}
		for event := range client.Events() {
			// Verify and apply the complete resource before saving its cursor or
			// sending ResourceApplied. Notifications alone are not applied state.
			fmt.Println(event.GetResourceRevision())
		}
		return client.Err()
	})
	if err != nil {
		fmt.Println(err)
	}
}
