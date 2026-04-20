package startup

import "time"

// DefaultShutdownTimeout is the fallback graceful shutdown deadline for services.
const DefaultShutdownTimeout = 10 * time.Second
