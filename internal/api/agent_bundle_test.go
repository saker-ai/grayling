package api

import (
	"errors"
	agentv1 "github.com/saker-ai/grayling/api/agent/v1"
	"github.com/saker-ai/grayling/internal/admin"
	"github.com/saker-ai/grayling/internal/platform"
	"net/http/httptest"
	"testing"
)

func TestBundleUnknownOutcomeTakesPrecedenceOverDomainErrors(t *testing.T) {
	for _, cause := range []error{platform.ErrConflict, platform.ErrAccessDenied, &agentv1.ProtocolError{Code: agentv1.ErrorCode_ERROR_CODE_STALE_REVISION}} {
		response := httptest.NewRecorder()
		writeBundlePublicationError(response, errors.Join(admin.ErrBundleOutcomeUnknown, cause))
		if response.Code != 503 {
			t.Fatal("audit error concealed unknown publication outcome", response.Code, cause)
		}
	}
	response := httptest.NewRecorder()
	writeBundlePublicationError(response, platform.ErrConflict)
	if response.Code != 409 {
		t.Fatal("definitive conflict changed", response.Code)
	}
}
