package requestarchive

import "encoding/json"

type usageFields struct {
	Prompt      *int64 `json:"prompt_tokens"`
	Input       *int64 `json:"input_tokens"`
	Completion  *int64 `json:"completion_tokens"`
	Output      *int64 `json:"output_tokens"`
	CacheRead   *int64 `json:"cache_read_input_tokens"`
	CacheCreate *int64 `json:"cache_creation_input_tokens"`
}
type geminiUsage struct {
	Prompt     *int64 `json:"promptTokenCount"`
	Candidates *int64 `json:"candidatesTokenCount"`
	Thoughts   *int64 `json:"thoughtsTokenCount"`
}
type usageEnvelope struct {
	Type     string         `json:"type"`
	Usage    usageFields    `json:"usage"`
	Gemini   geminiUsage    `json:"usageMetadata"`
	Message  *usageEnvelope `json:"message"`
	Response *usageEnvelope `json:"response"`
}
type usageTotals struct {
	input, output, cacheRead, cacheCreate, thoughts *int64
	gemini                                          bool
	inclusiveInput                                  bool
}

func setTotal(dst **int64, src *int64) {
	if src != nil && *src >= 0 {
		v := *src
		*dst = &v
	}
}
func (c *capture) observeUsage(raw []byte) {
	var envelope usageEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	c.observeEnvelope(&envelope, 0)
}
func (c *capture) observeEnvelope(e *usageEnvelope, depth int) {
	if e == nil || depth > 4 {
		return
	}
	if e.Type == "message_stop" || e.Type == "response.completed" {
		c.terminal = true
	}
	u := &c.usage
	if e.Usage.Prompt != nil {
		setTotal(&u.input, e.Usage.Prompt)
		u.inclusiveInput = true
	} else {
		setTotal(&u.input, e.Usage.Input)
	}
	if e.Usage.Completion != nil {
		setTotal(&u.output, e.Usage.Completion)
	} else {
		setTotal(&u.output, e.Usage.Output)
	}
	setTotal(&u.cacheRead, e.Usage.CacheRead)
	setTotal(&u.cacheCreate, e.Usage.CacheCreate)
	if e.Gemini.Prompt != nil || e.Gemini.Candidates != nil || e.Gemini.Thoughts != nil {
		u.gemini = true
		setTotal(&u.input, e.Gemini.Prompt)
		setTotal(&u.output, e.Gemini.Candidates)
		setTotal(&u.thoughts, e.Gemini.Thoughts)
	}
	c.observeEnvelope(e.Message, depth+1)
	c.observeEnvelope(e.Response, depth+1)
}
func (u usageTotals) totals(protocol string) (*int64, *int64) {
	var input, output *int64
	setTotal(&input, u.input)
	setTotal(&output, u.output)
	if input != nil && !u.inclusiveInput && protocol == "ANTHROPIC" {
		if u.cacheRead != nil {
			*input += *u.cacheRead
		}
		if u.cacheCreate != nil {
			*input += *u.cacheCreate
		}
	}
	if u.gemini && u.thoughts != nil {
		if output == nil {
			v := int64(0)
			output = &v
		}
		*output += *u.thoughts
	}
	return input, output
}
