package controller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Copy of parseEventMessage for testing
func parseEventMessageRepro(message string) (corev1.ResourceName, resource.Quantity, error) {
	parts := strings.Split(message, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "requested: ") {
			reqPart := strings.TrimPrefix(part, "requested: ")
			kv := strings.Split(reqPart, "=")
			if len(kv) == 2 {
				resName := corev1.ResourceName(kv[0])
				reqQty, err := resource.ParseQuantity(kv[1])
				if err == nil {
					return resName, reqQty, nil
				}
			}
		}
	}
	return "", resource.Quantity{}, fmt.Errorf("failed to parse message")
}

func TestParseEventMessage(t *testing.T) {
	msg := `create Pod burst-sts-0 in StatefulSet burst-sts failed error: pods "burst-sts-0" is forbidden: exceeded quota: sts-burst-quota, requested: requests.cpu=200m, used: requests.cpu=0, limited: requests.cpu=100m`

	resName, qty, err := parseEventMessageRepro(msg)
	assert.NoError(t, err)
	assert.Equal(t, corev1.ResourceName("requests.cpu"), resName)
	assert.Equal(t, int64(200), qty.MilliValue())
}
