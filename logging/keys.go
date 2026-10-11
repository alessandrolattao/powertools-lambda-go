package logging

// The keys of a Powertools log record, named as Powertools Python names
// them, so that a query written for a Python function reads a Go one too.
const (
	// KeyLevel holds the record's level name.
	KeyLevel = "level"
	// KeyLocation holds where the record was logged: function:line.
	KeyLocation = "location"
	// KeyMessage holds the message.
	KeyMessage = "message"
	// KeyTimestamp holds when the record was logged.
	KeyTimestamp = "timestamp"
	// KeyService holds the service name.
	KeyService = "service"
	// KeySamplingRate holds the debug sampling rate, when one is set.
	KeySamplingRate = "sampling_rate"
	// KeyColdStart tells whether the invocation is the cold one.
	KeyColdStart = "cold_start"
	// KeyFunctionName holds the Lambda function's name.
	KeyFunctionName = "function_name"
	// KeyFunctionMemorySize holds the memory configured for the function, in MB.
	KeyFunctionMemorySize = "function_memory_size"
	// KeyFunctionARN holds the ARN the function was invoked with.
	KeyFunctionARN = "function_arn"
	// KeyFunctionRequestID holds the invocation's request id.
	KeyFunctionRequestID = "function_request_id"
	// KeyCorrelationID holds the correlation id of the invocation.
	KeyCorrelationID = "correlation_id"
	// KeyXRayTraceID holds the X-Ray trace id of the invocation.
	KeyXRayTraceID = "xray_trace_id"
	// KeyException holds the logged error's message.
	KeyException = "exception"
	// KeyExceptionName holds the logged error's type name.
	KeyExceptionName = "exception_name"
	// KeyStackTrace holds the stack of a panic, or of an error that carries one.
	KeyStackTrace = "stack_trace"
)

// ErrorKey is the attribute key that carries the error of a record:
//
//	logger.ErrorContext(ctx, "charge failed", "err", err)
//
// An error under this key, outside any group, is written as the exception,
// exception_name and, when a stack is known, stack_trace keys.
const ErrorKey = "err"

// The JMESPath expressions that select the correlation id of the common
// event sources, Powertools Python's correlation_paths. Pass one to
// WithCorrelationIDPath.
const (
	CorrelationAPIGatewayREST          = "requestContext.requestId"
	CorrelationAPIGatewayHTTP          = CorrelationAPIGatewayREST
	CorrelationAppSyncAuthorizer       = "requestContext.requestId"
	CorrelationAppSyncResolver         = `request.headers."x-amzn-trace-id"`
	CorrelationApplicationLoadBalancer = `headers."x-amzn-trace-id"`
	CorrelationEventBridge             = "id"
	CorrelationLambdaFunctionURL       = CorrelationAPIGatewayREST
	CorrelationS3ObjectLambda          = "xAmzRequestId"
	CorrelationVPCLattice              = `headers."x-amzn-trace-id"`
)
