package eventsource

// The JSON samples in testdata are the sample events of Powertools for AWS
// Lambda (Python), tests/events, MIT No Attribution.

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sample(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

// decode reads a sample event into a T with encoding/json/v2 defaults: the
// way the parser package and Powertools middlewares decode events.
func decode[T any](t *testing.T, name string) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(sample(t, name), &out))
	return out
}

// viaLambda decodes a sample event into a T the way aws-lambda-go's
// lambda.Start does, with encoding/json v1, to prove the types work in a
// plain handler too.
func viaLambda[T any](t *testing.T, name string) T {
	t.Helper()
	var got T
	handler := lambda.NewHandler(func(_ context.Context, event T) error {
		got = event
		return nil
	})
	_, err := handler.Invoke(t.Context(), sample(t, name))
	require.NoError(t, err)
	return got
}

func TestVPCLattice(t *testing.T) {
	t.Parallel()

	v1 := decode[VPCLatticeEvent](t, "vpcLatticeEvent.json")
	assert.Equal(t, "/testpath", v1.RawPath)
	assert.Equal(t, "GET", v1.Method)
	assert.Equal(t, "1", v1.QueryStringParameters["order-id"])
	assert.True(t, v1.IsBase64Encoded)
	body, err := Body(v1)
	require.NoError(t, err)
	assert.JSONEq(t, `{"test": "event"}`, string(body))
	assert.Equal(t, []string{"value1", "value2"}, HeaderValues(v1, "Header2"))

	v2 := decode[VPCLatticeEventV2](t, "vpcLatticeV2Event.json")
	assert.Equal(t, "2.0", v2.Version)
	assert.Equal(t, "/newpath", v2.Path)
	assert.Equal(t, StringValues{"curl/7.64.1"}, v2.Headers["user_agent"])
	assert.Equal(t, "arn:aws:vpc-lattice:us-east-2:123456789012:service/svc-0a40eebed65f8d69c", v2.RequestContext.ServiceARN)
	assert.Equal(t, "AWS_IAM", v2.RequestContext.Identity.Type)
	assert.Equal(t, "example.com", v2.RequestContext.Identity.X509SANDNS)
	assert.Equal(t, EpochMicros(1696331543569073), v2.RequestContext.TimeEpoch)
	assert.Equal(t, time.Date(2023, 10, 3, 11, 12, 23, 569073000, time.UTC), v2.RequestContext.TimeEpoch.Time())

	multi := decode[VPCLatticeEventV2](t, "vpcLatticeV2EventWithHeaders.json")
	assert.Equal(t, StringValues{"value1", "value2"}, multi.Headers["Header2"])
	assert.Equal(t, []string{"value1", "value2"}, HeaderValues(multi, "header2"))
	value, ok := Header(multi, "header2")
	assert.True(t, ok)
	assert.Equal(t, "value1,value2", value)
	param, ok := QueryParameter(multi, "parameter1")
	assert.True(t, ok)
	assert.Equal(t, "value1,value2", param)
	assert.Equal(t, []string{"value1", "value2"}, QueryParameterValues(multi, "parameter1"))

	assert.Equal(t, v2.RequestContext.ServiceARN, viaLambda[VPCLatticeEventV2](t, "vpcLatticeV2Event.json").RequestContext.ServiceARN)
}

func TestAPIGatewayProxyRequest(t *testing.T) {
	t.Parallel()
	for _, got := range []APIGatewayProxyRequest{
		decode[APIGatewayProxyRequest](t, "apiGatewayProxyEvent.json"),
		viaLambda[APIGatewayProxyRequest](t, "apiGatewayProxyEvent.json"),
	} {
		assert.Equal(t, "1.0", got.Version)
		assert.Equal(t, "/my/path", got.Path)
		assert.Equal(t, "GET", got.HTTPMethod)
		assert.Equal(t, "123456789012", got.RequestContext.AccountID)
		assert.Equal(t, "192.168.0.1/32", got.RequestContext.Identity.SourceIP)
		assert.Empty(t, got.APIGatewayProxyRequest.RequestContext.AccountID, "the embedded context is shadowed")
	}
}

func TestAPIGatewayAuthorizerRequest(t *testing.T) {
	t.Parallel()

	rest := decode[APIGatewayCustomAuthorizerRequestTypeRequest](t, "apiGatewayAuthorizerRequestEvent.json")
	assert.Equal(t, "1.0", rest.Version)
	assert.Equal(t, "REQUEST", rest.Type)
	assert.Equal(t, "user1,123", rest.IdentitySource)
	assert.Equal(t, "user1,123", rest.AuthorizationToken)
	assert.Equal(t, "3npb9j1tlk.execute-api.us-west-1.amazonaws.com", rest.RequestContext.DomainName)
	assert.Equal(t, "HTTP/1.1", rest.RequestContext.Protocol)
	assert.Equal(t, int64(1629470210043), rest.RequestContext.RequestTimeEpoch)
	assert.Equal(t, "PostmanRuntime/7.28.3", rest.RequestContext.Identity.UserAgent)
	require.NotNil(t, rest.RequestContext.Identity.ClientCert)
	assert.Equal(t, "CERT_CONTENT", rest.RequestContext.Identity.ClientCert.ClientCertPem)

	ws := decode[APIGatewayCustomAuthorizerRequestTypeRequest](t, "apiGatewayAuthorizerWebSocketEvent.json")
	assert.Equal(t, "$connect", ws.RequestContext.RouteKey)
	assert.Equal(t, "CONNECT", ws.RequestContext.EventType)
	assert.Equal(t, "FDmBIeapIAMCIQg=", ws.RequestContext.ConnectionID)
	assert.Equal(t, int64(1737995987617), ws.RequestContext.ConnectedAt)
	assert.Equal(t, "IN", ws.RequestContext.MessageDirection)
	assert.Equal(t, "93.108.161.96", ws.RequestContext.Identity.SourceIP)

	arn, err := ParseWebSocketRouteARN(ws.MethodArn)
	require.NoError(t, err)
	assert.Equal(t, "$connect", arn.Resource)
}

func TestAppSync(t *testing.T) {
	t.Parallel()

	direct := decode[AppSyncResolverEvent](t, "appSyncDirectResolver.json")
	assert.Equal(t, "createSomething", direct.Field())
	assert.Equal(t, "Mutation", direct.ParentType())
	assert.Equal(t, []string{"id", "field1", "field2"}, direct.Info.SelectionSetList)
	assert.Equal(t, "my identifier", direct.Arguments["id"])
	require.NotNil(t, direct.Identity)
	assert.Equal(t, AppSyncAuthorizationCognito, direct.Identity.Authorization())
	assert.Equal(t, "jdoe", direct.Identity.Username)
	assert.Equal(t, []string{"1.1.1.1"}, direct.Identity.SourceIP)
	assert.Nil(t, direct.Prev)
	auth, ok := LookupHeader(direct.Request.Headers, "Authorization")
	assert.True(t, ok)
	assert.Equal(t, "eyJraWQiOiJScWFCSlJqYVJlM0hrSnBTUFpIcVRXazNOW...", auth)

	amplify := decode[AppSyncResolverEvent](t, "appSyncResolverEvent.json")
	assert.Equal(t, "locations", amplify.Field())
	assert.Equal(t, "Merchant", amplify.ParentType())
	require.NotNil(t, amplify.Prev)
	assert.Equal(t, map[string]any{}, amplify.Prev.Result)
	assert.Equal(t, "Value", amplify.Source["name"])

	custom := decode[AppSyncResolverEvent](t, "appSyncCustomResolverEvent.json")
	assert.Equal(t, "Merchant", custom.ParentType())

	channel := decode[AppSyncEventsEvent](t, "appSyncEventsEvent.json")
	assert.Nil(t, channel.Identity)
	assert.Equal(t, AppSyncEventsPublish, channel.Info.Operation)
	assert.Equal(t, "/default/channel", channel.Info.Channel.Path)
	assert.Equal(t, []string{"default", "channel"}, channel.Info.Channel.Segments)
	assert.Equal(t, "default", channel.Info.ChannelNamespace.Name)
	require.Len(t, channel.Events, 2)
	assert.Equal(t, "1", channel.Events[0].ID)
	assert.Equal(t, map[string]any{"event_1": "data_1"}, channel.Events[0].Payload)
	require.NotNil(t, channel.Prev, "a placeholder prev decodes as empty")
	assert.Nil(t, channel.Prev.Result)
}

func TestAppSyncIdentityAuthorization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		identity string
		want     AppSyncAuthorization
	}{
		{"cognito", `{"sub":"s","username":"u","defaultAuthStrategy":"ALLOW","claims":{}}`, AppSyncAuthorizationCognito},
		{"oidc", `{"sub":"s","issuer":"https://id.example.com","claims":{}}`, AppSyncAuthorizationOIDC},
		{"lambda", `{"resolverContext":{"tenant":"t1"}}`, AppSyncAuthorizationLambda},
		{"iam", `{"accountId":"123","userArn":"arn:aws:iam::123:user/u","username":"u"}`, AppSyncAuthorizationIAM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var identity AppSyncIdentity
			require.NoError(t, json.Unmarshal([]byte(tc.identity), &identity))
			assert.Equal(t, tc.want, identity.Authorization())
		})
	}
}

func TestBedrockAgent(t *testing.T) {
	t.Parallel()

	get := decode[BedrockAgentEvent](t, "bedrockAgentEvent.json")
	assert.Equal(t, "1.0", get.MessageVersion)
	assert.Equal(t, "GET", get.HTTPMethod)
	assert.NotEmpty(t, get.APIPath)
	assert.NotEmpty(t, get.Agent.Name)
	_, hasBody := get.BodyProperties()
	assert.False(t, hasBody)

	post := decode[BedrockAgentEvent](t, "bedrockAgentPostEvent.json")
	assert.Equal(t, "/send-reminders", post.APIPath)
	assert.Equal(t, "TSTALIASID", post.Agent.Alias)
	props, ok := post.BodyProperties()
	require.True(t, ok)
	assert.Equal(t, map[string]string{"claimId": "20", "pendingDocuments": "social number and vat"}, props)
	assert.Empty(t, post.QueryParameters())

	fn := decode[BedrockAgentFunctionEvent](t, "bedrockAgentFunctionEvent.json")
	assert.Equal(t, "submitVacationRequest", fn.Function)
	assert.Equal(t, "VacationsActionGroup", fn.ActionGroup)
	assert.Equal(t, "EMP123", fn.SessionAttributes["employeeId"])
	assert.Equal(t, "vacation", fn.PromptSessionAttributes["requestType"])
	require.Len(t, fn.Parameters, 2)
	assert.Equal(t, BedrockAgentProperty{Name: "start_date", Type: "string", Value: "2024-03-15"}, fn.Parameters[0])
}

func TestCloudWatchAlarm(t *testing.T) {
	t.Parallel()

	single := decode[CloudWatchAlarmEvent](t, "cloudWatchAlarmEventSingleMetric.json")
	assert.Equal(t, "aws.cloudwatch", single.Source)
	assert.Equal(t, "Test alert", single.AlarmData.AlarmName)
	assert.Equal(t, CloudWatchAlarmStateAlarm, single.AlarmData.State.Value)
	assert.Equal(t, CloudWatchAlarmStateOK, single.AlarmData.PreviousState.Value)
	require.Len(t, single.AlarmData.Configuration.Metrics, 3)
	assert.Equal(t, "m1/m2", single.AlarmData.Configuration.Metrics[0].Expression)
	require.NotNil(t, single.AlarmData.Configuration.Metrics[1].MetricStat)
	assert.Equal(t, "Invocations", single.AlarmData.Configuration.Metrics[1].MetricStat.Metric.Name)
	assert.Equal(t, int64(60), single.AlarmData.Configuration.Metrics[1].MetricStat.Period)

	when, err := ParseCloudWatchTime(single.Time)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2024, 2, 17, 11, 53, 8, 431000000, time.UTC), when.UTC())

	reason, err := single.AlarmData.State.DecodeReasonData[map[string]any]()
	require.NoError(t, err)
	assert.Equal(t, "SampleCount", reason["statistic"])

	composite := decode[CloudWatchAlarmEvent](t, "cloudWatchAlarmEventCompositeMetric.json")
	assert.Equal(t, "WaitPeriod", composite.AlarmData.PreviousState.ActionsSuppressedBy)
	assert.Equal(t, int64(120), composite.AlarmData.Configuration.ActionsSuppressorWaitPeriod)
	assert.Equal(t, int64(180), composite.AlarmData.Configuration.ActionsSuppressorExtensionPeriod)

	empty, err := CloudWatchAlarmState{}.DecodeReasonData[map[string]any]()
	require.NoError(t, err)
	assert.Nil(t, empty)
	_, err = CloudWatchAlarmState{ReasonData: "{"}.DecodeReasonData[map[string]any]()
	require.Error(t, err)
	_, err = ParseCloudWatchTime("yesterday")
	require.Error(t, err)
}

func TestCloudWatchDashboardCustomWidget(t *testing.T) {
	t.Parallel()
	got := decode[CloudWatchDashboardCustomWidgetEvent](t, "cloudWatchDashboardEvent.json")
	assert.False(t, got.Describe)
	require.NotNil(t, got.WidgetContext)
	ctx := got.WidgetContext
	assert.Equal(t, "Name-of-current-dashboard", ctx.DashboardName)
	assert.Equal(t, int64(300), ctx.Period)
	assert.True(t, ctx.IsAutoPeriod)
	assert.Equal(t, "+00:00", ctx.Timezone.OffsetISO)
	assert.Equal(t, int64(86400012), ctx.TimeRange.RelativeStart)
	require.NotNil(t, ctx.TimeRange.Zoom)
	assert.Equal(t, int64(1627276030434), ctx.TimeRange.Zoom.Start)
	assert.Equal(t, map[string]any{}, ctx.Forms.All)
	assert.Equal(t, int64(588), ctx.Width)
	assert.JSONEq(t, `{"original":"param-to-widget"}`, string(got.Params))
}

func TestCloudWatchLogs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		policyLevel string
	}{
		{"cloudWatchLogEvent.json", ""},
		{"cloudWatchLogEventWithPolicyLevel.json", "ACCOUNT_LEVEL_POLICY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			event := decode[events.CloudwatchLogsEvent](t, tc.name)
			data, err := ParseCloudWatchLogs(event.AWSLogs)
			require.NoError(t, err)
			assert.Equal(t, "testLogGroup", data.LogGroup)
			assert.Equal(t, []string{"testFilter"}, data.SubscriptionFilters)
			assert.Equal(t, tc.policyLevel, data.PolicyLevel)
			require.Len(t, data.LogEvents, 2)
			assert.Equal(t, "eventId1", data.LogEvents[0].ID)
			assert.Equal(t, "[ERROR] First test message", data.LogEvents[0].Message)
		})
	}
}

func TestCodeDeployLifecycleHook(t *testing.T) {
	t.Parallel()
	got := decode[CodeDeployLifecycleHookEvent](t, "codeDeployLifecycleHookEvent.json")
	assert.Equal(t, CodeDeployLifecycleHookEvent{DeploymentID: "d-ABCDEF", LifecycleEventHookExecutionID: "xxxxxxxxxxxxxxxxxxxxxxxx"}, got)
}

func TestCognito(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"cognitoCustomEmailSenderEvent.json", "cognitoCustomSMSSenderEvent.json"} {
		got := decode[CognitoEventUserPoolsCustomSender](t, name)
		assert.Equal(t, "customEmailSenderRequestV1", got.Request.Type)
		assert.Equal(t, "someCode", got.Request.Code)
		assert.Equal(t, true, got.Request.UserAttributes["email_verified"])
		assert.Equal(t, "userPoolId", got.UserPoolID)
		assert.Equal(t, "clientId", got.CallerContext.ClientID)
	}

	migrate := decode[CognitoEventUserPoolsMigrateUser](t, "cognitoUserMigrationEvent.json")
	assert.Equal(t, "UserMigration_Authentication", migrate.TriggerSource)
	assert.Equal(t, "password", migrate.Password)
	migrate.Response.EnableSMSMFA = true
	migrate.Response.FinalUserStatus = "CONFIRMED"
	out, err := json.Marshal(migrate)
	require.NoError(t, err)
	var reply map[string]any
	require.NoError(t, json.Unmarshal(out, &reply))
	assert.Equal(t, map[string]any{
		"enableSMSMFA": true, "finalUserStatus": "CONFIRMED", "userAttributes": map[string]any{},
		"messageAction": "", "desiredDeliveryMediums": []any{}, "forceAliasCreation": false,
	}, reply["response"])

	message := decode[CognitoEventUserPoolsCustomMessage](t, "cognitoCustomMessageEvent.json")
	assert.Equal(t, "{##Click Here##}", message.Request.LinkParameter)
	assert.Equal(t, "####", message.Request.CodeParameter)
}

func TestConfig(t *testing.T) {
	t.Parallel()

	changed := decode[ConfigEvent](t, "awsConfigRuleConfigurationChanged.json")
	assert.Equal(t, "DETECTIVE", changed.EvaluationMode)
	assert.Equal(t, "MyRule", changed.ConfigRuleName)
	invoking, err := changed.DecodeInvokingEvent()
	require.NoError(t, err)
	item, ok := invoking.(*ConfigConfigurationChanged)
	require.True(t, ok)
	assert.Equal(t, ConfigConfigurationItemChangeNotification, item.MessageType())
	assert.Equal(t, "AWS::EC2::Instance", item.ConfigurationItem.ResourceType)
	assert.Equal(t, int64(1682607791636), item.ConfigurationItem.ConfigurationStateID)
	assert.Equal(t, "t2.medium", item.ConfigurationItem.Configuration["instanceType"])
	params, err := changed.DecodeRuleParameters[map[string]string]()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"desiredInstanceType": "t2.micro"}, params)

	oversized := decode[ConfigEvent](t, "awsConfigRuleOversizedConfiguration.json")
	invoking, err = oversized.DecodeInvokingEvent()
	require.NoError(t, err)
	summary, ok := invoking.(*ConfigOversizedConfiguration)
	require.True(t, ok)
	assert.Equal(t, "i-00000000", summary.ConfigurationItemSummary.ResourceID)
	assert.Equal(t, ConfigOversizedConfigurationItemChangeNotification, summary.MessageType())

	scheduled := decode[ConfigEvent](t, "awsConfigRuleScheduled.json")
	invoking, err = scheduled.DecodeInvokingEvent()
	require.NoError(t, err)
	schedule, ok := invoking.(*ConfigScheduled)
	require.True(t, ok)
	assert.Equal(t, "0123456789012", schedule.AWSAccountID)
	assert.Equal(t, ConfigScheduledNotification, schedule.MessageType())

	_, err = ConfigEvent{}.DecodeInvokingEvent()
	require.Error(t, err)
	none, err := ConfigEvent{}.DecodeRuleParameters[map[string]string]()
	require.NoError(t, err)
	assert.Nil(t, none)
}

func TestConnect(t *testing.T) {
	t.Parallel()

	all := decode[ConnectEvent](t, "connectContactFlowEventAll.json")
	data := all.Details.ContactData
	assert.Equal(t, ConnectChannelVoice, data.Channel)
	assert.Equal(t, ConnectInitiationAPI, data.InitiationMethod)
	require.NotNil(t, data.CustomerEndpoint)
	assert.Equal(t, ConnectEndpointTelephoneNumber, data.CustomerEndpoint.Type)
	require.NotNil(t, data.Queue)
	assert.Equal(t, "QueueOne", data.Queue.Name)
	assert.Equal(t, "1565781909613", data.MediaStreams.Customer.Audio.StartTimestamp)
	assert.Equal(t, "One", all.Details.Parameters["ParameterOne"])
	assert.Equal(t, "ContactFlowEvent", all.Name)

	minimal := decode[ConnectEvent](t, "connectContactFlowEventMin.json")
	assert.Nil(t, minimal.Details.ContactData.CustomerEndpoint)
	assert.Nil(t, minimal.Details.ContactData.Queue)
	assert.Nil(t, minimal.Details.ContactData.SystemEndpoint)
}

func TestEventBridge(t *testing.T) {
	t.Parallel()

	plain := decode[EventBridgeEvent](t, "eventBridgeEvent.json")
	assert.Equal(t, "EC2 Instance State-change Notification", plain.DetailType)
	assert.Empty(t, plain.ReplayName)
	replayed := EventBridgeEvent{}
	require.NoError(t, json.Unmarshal([]byte(`{"replay-name":"r1","detail":{}}`), &replayed))
	assert.Equal(t, "r1", replayed.ReplayName)

	created := decode[S3EventBridgeNotificationEvent](t, "s3EventBridgeNotificationObjectCreatedEvent.json")
	assert.Equal(t, "Object Created", created.DetailType)
	assert.Equal(t, "example-bucket", created.Detail.Bucket.Name)
	assert.Equal(t, "IMG_m7fzo3.jpg", created.Detail.Object.Key)
	assert.Equal(t, int64(184662), created.Detail.Object.Size)
	assert.Equal(t, "PutObject", created.Detail.Reason)

	deleted := decode[S3EventBridgeNotificationEvent](t, "s3EventBridgeNotificationObjectDeletedEvent.json")
	assert.Equal(t, "Delete Marker Created", deleted.Detail.DeletionType)
	assert.Empty(t, deleted.Detail.Object.ETag)
	assert.Empty(t, deleted.Detail.Object.VersionID)

	expired := decode[S3EventBridgeNotificationEvent](t, "s3EventBridgeNotificationObjectExpiredEvent.json")
	assert.Equal(t, "Lifecycle Expiration", expired.Detail.Reason)

	restored := decode[S3EventBridgeNotificationEvent](t, "s3EventBridgeNotificationObjectRestoreCompletedEvent.json")
	assert.Equal(t, "GLACIER", restored.Detail.SourceStorageClass)
	assert.Equal(t, "2021-11-13T00:00:00Z", restored.Detail.RestoreExpiryTime)

	key := S3EventBridgeNotificationObject{Key: "my+photo%2B1.jpg"}
	assert.Equal(t, "my photo+1.jpg", key.URLDecodedKey())
}

func TestIoTRegistry(t *testing.T) {
	t.Parallel()

	thing := decode[IoTCoreThingEvent](t, "iotRegistryEventsThingEvent.json")
	assert.Equal(t, IoTThingEvent, thing.EventType)
	assert.Equal(t, IoTOperationCreated, thing.Operation)
	assert.Equal(t, "MyThing", thing.ThingName)
	assert.Equal(t, int64(1), thing.VersionNumber)
	assert.Empty(t, thing.ThingTypeName)
	assert.Equal(t, "value1", thing.Attributes["attribute1"])
	assert.Equal(t, time.Date(2009, 2, 13, 23, 31, 30, 123000000, time.UTC), thing.Time())

	thingType := decode[IoTCoreThingTypeEvent](t, "iotRegistryEventsThingTypeEvent.json")
	assert.Equal(t, "MyThingType", thingType.ThingTypeName)
	assert.False(t, thingType.IsDeprecated)
	assert.Equal(t, []string{"attribute1", "attribute2", "attribute3"}, thingType.SearchableAttributes)
	assert.Equal(t, "model", thingType.PropagatingAttributes[0]["thingAttribute"])

	association := decode[IoTCoreThingTypeAssociationEvent](t, "iotRegistryEventsThingTypeAssociationEvent.json")
	assert.Equal(t, IoTThingTypeAssociationEvent, association.EventType)
	assert.Equal(t, "myThing", association.ThingName)

	group := decode[IoTCoreThingGroupEvent](t, "iotRegistryEventsThingGroupEvent.json")
	assert.Equal(t, "Tg_level5", group.ThingGroupName)
	assert.Equal(t, "Tg_level4", group.ParentGroupName)
	require.Len(t, group.RootToParentThingGroups, 5)
	assert.Equal(t, "36aa0482-f80d-4e13-9bff-1c0a75c055f6", group.RootToParentThingGroups[0]["groupId"])
	assert.Empty(t, group.DynamicGroupMappingID)

	membership := decode[IoTCoreAddOrRemoveFromThingGroupEvent](t, "iotRegistryEventsAddOrRemoveFromThingGroupEvent.json")
	assert.Equal(t, IoTThingGroupMembershipEvent, membership.EventType)
	assert.Equal(t, IoTOperationAdded, membership.Operation)
	assert.Equal(t, "8505ebf8-4d32-4286-80e9-c23a4a16bbd8", membership.MembershipID)

	hierarchy := decode[IoTCoreAddOrDeleteFromThingGroupEvent](t, "iotRegistryEventsAddOrDeleteFromThingGroupEvent.json")
	assert.Equal(t, IoTThingGroupHierarchyEvent, hierarchy.EventType)
	assert.Equal(t, "MyChildThingGroup", hierarchy.ChildGroupName)

	seconds := IoTRegistryEvent{Timestamp: 1603995417}
	assert.Equal(t, time.Date(2020, 10, 29, 18, 16, 57, 0, time.UTC), seconds.Time())
}

func TestTimeWindows(t *testing.T) {
	t.Parallel()

	kinesis := decode[KinesisTimeWindowEvent](t, "kinesisStreamTumblingWindowEvent.json")
	assert.Equal(t, map[string]any{"1": 282.0, "2": 715.0}, kinesis.State)
	assert.Equal(t, "shardId-000000000006", kinesis.ShardID)
	assert.False(t, kinesis.IsFinalInvokeForWindow)
	require.Len(t, kinesis.Records, 1)
	assert.Equal(t, "Hello, this is a test.", string(kinesis.Records[0].Kinesis.Data))
	assert.Equal(t, kinesis.State, viaLambda[KinesisTimeWindowEvent](t, "kinesisStreamTumblingWindowEvent.json").State)

	dynamo := decode[DynamoDBTimeWindowEvent](t, "dynamoStreamTumblingWindowEvent.json")
	assert.NotEmpty(t, dynamo.ShardID)
	assert.NotEmpty(t, dynamo.Records)

	response := KinesisTimeWindowEventResponse{State: map[string]any{"total": 997}}
	out, err := json.Marshal(response)
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":{"total":997},"batchItemFailures":[]}`, string(out))
}

func TestActiveMQ(t *testing.T) {
	t.Parallel()
	got := decode[ActiveMQEvent](t, "activeMQEvent.json")
	assert.Equal(t, "aws:amq", got.EventSource)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, "myJMSCoID", got.Messages[0].ConnectionID)
	assert.Equal(t, "testQueue", got.Messages[0].Destination.PhysicalName)
	data, err := got.Messages[0].DecodedData()
	require.NoError(t, err)
	assert.Equal(t, "ABC:AAAA", string(data))
	_, err = DecodeMQData("not base64!")
	require.Error(t, err)
}

func TestS3ObjectLambda(t *testing.T) {
	t.Parallel()
	got := decode[S3ObjectLambdaEvent](t, "s3ObjectEventIAMUser.json")
	assert.NotEmpty(t, got.UserIdentity.UserName)
	assert.Equal(t, "IAMUser", got.UserIdentity.Type)
	require.NotNil(t, got.GetObjectContext)
	assert.NotEmpty(t, got.GetObjectContext.OutputRoute)
}

func TestSES(t *testing.T) {
	t.Parallel()
	got := decode[SimpleEmailEvent](t, "sesEvent.json")
	require.Len(t, got.Records, 1)
	mail := got.Records[0].SES.Mail
	assert.Equal(t, "janedoe@example.com", mail.Source)
	assert.Equal(t, []string{"Jane Doe <janedoe@example.com>"}, mail.CommonHeaders.From)
	assert.Equal(t, "Test Subject", mail.CommonHeaders.Subject)
	assert.Equal(t, "PASS", got.Records[0].SES.Receipt.SPFVerdict.Status)

	var headers SimpleEmailCommonHeaders
	require.NoError(t, json.Unmarshal([]byte(`{"bcc":["b@example.com"],"sender":["s@example.com"],"replyTo":["r@example.com"],"subject":"x"}`), &headers))
	assert.Equal(t, []string{"b@example.com"}, headers.Bcc)
	assert.Equal(t, []string{"s@example.com"}, headers.Sender)
	assert.Equal(t, []string{"r@example.com"}, headers.ReplyTo)
	assert.Equal(t, "x", headers.Subject)
}

func TestTransferFamilyAuthorizer(t *testing.T) {
	t.Parallel()
	got := decode[TransferFamilyAuthorizerEvent](t, "transferFamilyAuthorizer.json")
	assert.Equal(t, "value", got.Username)
	require.NotNil(t, got.Password)
	assert.Equal(t, "value", *got.Password)
	assert.Equal(t, "SFTP", got.Protocol)
	assert.Equal(t, "s-abcd123456", got.ServerID)
	assert.Equal(t, "192.168.0.100", got.SourceIP)
}

func TestCloudFormationCustomResource(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cloudformationCustomResourceCreate.json", "cloudformationCustomResourceUpdate.json"} {
		got := decode[CloudFormationCustomResourceEvent](t, name)
		assert.NotEmpty(t, got.ServiceToken)
		assert.NotEmpty(t, got.ResponseURL)
		assert.NotEmpty(t, got.RequestType)
	}
	update := viaLambda[CloudFormationCustomResourceEvent](t, "cloudformationCustomResourceUpdate.json")
	assert.NotEmpty(t, update.OldResourceProperties)
}
