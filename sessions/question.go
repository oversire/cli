package sessions

import "strings"

// TurnQuestion gives the last paragraph of a reply whose last sentence is a question. A "?" in a code block does not count.
// The same rule as AgentSession.question(in:) in the app.
func TurnQuestion(message string) string {
	var prose []string
	code := false
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			code = !code
			prose = append(prose, "")
		} else if code {
			prose = append(prose, "")
		} else {
			prose = append(prose, line)
		}
	}
	if code {
		return ""
	}
	var last string
	for _, p := range strings.Split(strings.Join(prose, "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			last = p
		}
	}
	if last == "" {
		return ""
	}
	r := []rune(last)
	if !strings.HasSuffix(strings.TrimSpace(message), string(r[len(r)-1])) {
		return ""
	}
	if strings.HasSuffix(strings.Trim(last, "*_\"')”’ "), "?") {
		return last
	}
	return ""
}
