package telegram

import (
	"fmt"
	"net"
	"net/http"

	"github.com/aws/aws-lambda-go/events"
)

// telegramCIDRs contains the official IP ranges for Telegram servers.
// Reference: https://core.telegram.org/resources/cidr.txt
var telegramCIDRs = []string{
	"91.108.56.0/22",
	"91.108.4.0/22",
	"91.108.8.0/22",
	"91.108.16.0/22",
	"91.108.12.0/22",
	"149.154.160.0/20",
	"91.105.192.0/23",
	"91.108.20.0/22",
	"185.76.151.0/24",
	"2001:b28:f23d::/48",
	"2001:b28:f23f::/48",
	"2001:67c:4e8::/48",
	"2001:b28:f23c::/48",
	"2a0a:f280::/32",
}

type IPValidator struct {
	subnets []*net.IPNet
}

func NewIPValidator() (*IPValidator, error) {
	v := &IPValidator{}
	for _, cidr := range telegramCIDRs {
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %s: %w", cidr, err)
		}
		v.subnets = append(v.subnets, subnet)
	}
	return v, nil
}

// IsFromTelegram checks if the given IP string is within Telegram's IP ranges.
func (v *IPValidator) IsFromTelegram(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	for _, subnet := range v.subnets {
		if subnet.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateRequest performs IP validation on the Lambda request.
func (v *IPValidator) ValidateRequest(event events.LambdaFunctionURLRequest) (bool, events.LambdaFunctionURLResponse) {
	sourceip := event.RequestContext.HTTP.SourceIP
	if v.IsFromTelegram(sourceip) {
		return true, events.LambdaFunctionURLResponse{}
	}

	return false, events.LambdaFunctionURLResponse{
		StatusCode: http.StatusForbidden,
		Body:       fmt.Sprintf("Forbidden: IP %s is not in Telegram range", sourceip),
	}
}
