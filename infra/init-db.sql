-- Initialize database with required extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS vector;

-- Verify extensions are loaded
SELECT extname FROM pg_extension WHERE extname IN ('uuid-ossp', 'vector');
