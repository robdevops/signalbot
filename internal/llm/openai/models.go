package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Capabilities asks the provider's model list whether the model accepts
// images and tool calls. If it cannot tell, images are off (sending them
// would fail every such message) and tools stay on, and the failure is
// logged.
func (c *Client) Capabilities(ctx context.Context) (images, tools bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.opt.URL+"/models", nil)
	if c.opt.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.opt.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		logx.Warnf("could not check what %s supports, so images are off: %v", c.opt.Model, err)
		return false, true
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			ID           string `json:"id"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&r) != nil {
		logx.Warnf("could not check what %s supports, so images are off: status %d", c.opt.Model, resp.StatusCode)
		return false, true
	}
	for _, m := range r.Data {
		if m.ID == c.opt.Model {
			return slices.Contains(m.Architecture.InputModalities, "image"), slices.Contains(m.SupportedParameters, "tools")
		}
	}
	logx.Warnf("model %s is not in %s's model list, so images are off", c.opt.Model, c.name())
	return false, true
}
