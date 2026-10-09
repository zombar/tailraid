package chunk

// MemoryUsed is the size of the in-memory tier (tests only).
func (c *Cache) MemoryUsed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memUsed
}
