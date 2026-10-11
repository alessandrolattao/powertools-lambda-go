package eventsource

// CodeDeployLifecycleHookEvent is what CodeDeploy sends to a lifecycle event
// hook function (BeforeAllowTraffic, AfterAllowTraffic, ...). The function
// reports the outcome with the CodeDeploy PutLifecycleEventHookExecutionStatus
// API.
type CodeDeployLifecycleHookEvent struct {
	DeploymentID                  string `json:"DeploymentId"`
	LifecycleEventHookExecutionID string `json:"LifecycleEventHookExecutionId"`
}
