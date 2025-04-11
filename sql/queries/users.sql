-- name: CreateUser :one
INSERT INTO users (id, created_at, updated_at, email, hashed_password)
VALUES (
    gen_random_uuid(),
    NOW(),
    NOW(),
    $1, -- email 
    $2  -- hashed_password
)
RETURNING *;

-- name: GetUserByID :one
SELECT id, email, hashed_password, created_at, updated_at
FROM users
WHERE id = $1;

-- name: GetUserByEmail :one
SELECT id, email, hashed_password, created_at, updated_at
FROM users
WHERE email = $1;

-- name: UpdateUserByID :exec
UPDATE users
SET 
    email = COALESCE($1, email),
    hashed_password = COALESCE($2, hashed_password),
    updated_at = NOW()
WHERE id = $3;

-- name: DeleteAllUsers :exec
DELETE FROM users;

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (token, user_id, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetRefreshToken :one
SELECT * FROM refresh_tokens
WHERE token = $1
LIMIT 1;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens 
SET revoked_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE token = $1;