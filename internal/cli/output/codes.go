package output

const Schema = "eru/v1"

const EventSchema = "eru-event/v1"

const (
	CodeUsage                 = "usage_error"
	CodeOutputMode            = "invalid_output_mode"
	CodeContextNotFound       = "context_not_found"
	CodeClusterNotInitialized = "cluster_not_initialized"
	CodeMissingTool           = "missing_tool"
	CodeUnsafeAction          = "unsafe_action_requires_confirmation"
	CodeStepFailed            = "step_failed"
	CodePartialFailure        = "partial_failure"
	CodeDoctorFailed          = "doctor_failed"
	CodeExternalCommand       = "external_command_failed"
	CodeConfig                = "config_error"
	CodeValidation            = "validation_failed"
	CodeAmbiguous             = "ambiguous_input"
	CodeInputRequired         = "input_required"
	CodeRemoteUnreachable     = "remote_unreachable"
	CodeTransportFailed       = "transport_failed"
	CodeHostKeyMismatch       = "host_key_mismatch"
	CodeEndpointUnreachable   = "endpoint_unreachable"
	CodeBucketNotFound        = "bucket_not_found"
	CodeBackupAuthFailed      = "backup_auth_failed"
	CodeNodeVersionMismatch   = "node_version_mismatch"
	CodeUpdateRequired        = "update_required"
	CodePrecondition          = "precondition_failed"
	CodeLockdownFailed        = "lockdown_failed"
	CodeSecrets               = "secrets_error"
	CodeReleaseVerification   = "release_verification_failed"
	CodeDatabaseProtected     = "database_protected"
	CodeProtectCodeRejected   = "protect_code_rejected"
	CodeError                 = "error"
)

const (
	ExitOK                  = 0
	ExitUsage               = 1
	ExitContext             = 2
	ExitMissingTool         = 3
	ExitUnsafe              = 4
	ExitStep                = 5
	ExitExternal            = 6
	ExitConfig              = 7
	ExitInput               = 8
	ExitTransport           = 9
	ExitVersion             = 10
	ExitPrecondition        = 11
	ExitLockdown            = 12
	ExitSecrets             = 13
	ExitReleaseVerification = 14
	ExitGeneric             = 70
)

var exitByCode = map[string]int{
	CodeUsage:                 ExitUsage,
	CodeOutputMode:            ExitUsage,
	CodeContextNotFound:       ExitContext,
	CodeClusterNotInitialized: ExitContext,
	CodeMissingTool:           ExitMissingTool,
	CodeUnsafeAction:          ExitUnsafe,
	CodeStepFailed:            ExitStep,
	CodePartialFailure:        ExitStep,
	CodeDoctorFailed:          ExitStep,
	CodeExternalCommand:       ExitExternal,
	CodeConfig:                ExitConfig,
	CodeValidation:            ExitConfig,
	CodeAmbiguous:             ExitInput,
	CodeInputRequired:         ExitInput,
	CodeRemoteUnreachable:     ExitTransport,
	CodeTransportFailed:       ExitTransport,
	CodeHostKeyMismatch:       ExitTransport,
	CodeEndpointUnreachable:   ExitTransport,
	CodeBucketNotFound:        ExitTransport,
	CodeBackupAuthFailed:      ExitTransport,
	CodeNodeVersionMismatch:   ExitVersion,
	CodeUpdateRequired:        ExitVersion,
	CodePrecondition:          ExitPrecondition,
	CodeLockdownFailed:        ExitLockdown,
	CodeSecrets:               ExitSecrets,
	CodeReleaseVerification:   ExitReleaseVerification,
	CodeDatabaseProtected:     ExitUnsafe,
	CodeProtectCodeRejected:   ExitUnsafe,
	CodeError:                 ExitGeneric,
}

// IsKnownCode reports whether code is a stable eru error code.
func IsKnownCode(code string) bool {
	_, ok := exitByCode[code]
	return ok
}

// ExitFor returns the exit status for a stable error code, or ExitGeneric for
// an unknown one. The operator uses it for errors relayed from a node.
func ExitFor(code string) int {
	if exit, ok := exitByCode[code]; ok {
		return exit
	}
	return ExitGeneric
}
