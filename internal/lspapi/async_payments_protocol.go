package lspapi

const (
	asyncOrderProtocolVersion uint64 = 1
	asyncHashPoolMaxSize             = 200
	asyncOrderJSONRPCVersion         = "2.0"

	asyncOrderJSONRPCParseError               = -32700
	asyncOrderJSONRPCInvalidRequest           = -32600
	asyncOrderJSONRPCInternalError            = -32603
	asyncOrderErrorUnsupportedProtocolVersion = 1000
	asyncOrderErrorInvalidHashBatch           = 1003
	asyncOrderErrorDuplicateIndexConflict     = 1004
	asyncOrderErrorDuplicateHashConflict      = 1005
)
