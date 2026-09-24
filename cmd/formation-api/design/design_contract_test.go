// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design_test

import (
	"testing"

	httpclient "github.com/linuxfoundation/lfx-v2-formation-service/gen/http/lfx_v2_formation_service/client"
	"github.com/stretchr/testify/require"
)

func TestApplicationPayloadTooLargeIsADeclaredReason(t *testing.T) {
	name := "bad_request"
	code := "400"
	message := "application submission exceeds the transport-safe size limit"
	reason := "application_payload_too_large"

	err := httpclient.ValidateCreateApplicationBadRequestResponseBody(
		&httpclient.CreateApplicationBadRequestResponseBody{
			Name: &name, Code: &code, Message: &message, Reason: &reason,
		},
	)

	require.NoError(t, err)
}

func TestApplicationFieldInvalidIsADeclaredReason(t *testing.T) {
	name := "bad_request"
	code := "400"
	message := "an application field has an invalid value"
	reason := "application_field_invalid"

	err := httpclient.ValidateCreateApplicationBadRequestResponseBody(
		&httpclient.CreateApplicationBadRequestResponseBody{
			Name: &name, Code: &code, Message: &message, Reason: &reason,
		},
	)

	require.NoError(t, err)
}
