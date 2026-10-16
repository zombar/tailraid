package webui

// NeedBytes is needBytes, for the boundary table in the tests.
var NeedBytes = needBytes

// RenderMarkdown is renderMarkdown, for the renderer's tables and fuzz target.
var RenderMarkdown = renderMarkdown

// IndexCSP is the index page's Content-Security-Policy (I-315).
const IndexCSP = indexCSP

// HoldDecoders takes every decode slot until release: a thumbnail being made waits there.
func (s *Server) HoldDecoders() (release func()) {
	for range cap(s.decoders) {
		s.decoders <- struct{}{}
	}
	return func() {
		for range cap(s.decoders) {
			<-s.decoders
		}
	}
}

// ThumbJobsRunning is how many background thumbnail jobs run now (I-374).
func (s *Server) ThumbJobsRunning() int {
	s.thumbs.mu.Lock()
	defer s.thumbs.mu.Unlock()
	return s.thumbs.running
}
