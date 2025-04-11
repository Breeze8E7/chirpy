package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/breeze/chirpy/internal/auth"
	"github.com/breeze/chirpy/internal/database"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file")
	}
	dbURL := os.Getenv("DB_URL")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Error opening database connection: %v", err)
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		log.Fatal("JWT_SECRET environment variable is not set")
	}
	dbQueries := database.New(db)
	apiCfg := &apiConfig{
		DB:        dbQueries,
		Platform:  os.Getenv("PLATFORM"),
		jwtSecret: jwtSecret,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", healthHandler)
	mux.HandleFunc("GET /admin/metrics", apiCfg.metricsHandler)
	mux.HandleFunc("GET /api/chirps", apiCfg.getAllChirps)
	mux.HandleFunc("GET /api/chirps/{chirpID}", apiCfg.getOneChirp)
	mux.HandleFunc("POST /admin/reset", apiCfg.resetHandler)
	mux.HandleFunc("POST /api/chirps", apiCfg.newChirp)
	mux.HandleFunc("POST /api/users", apiCfg.handleNewUser)
	mux.HandleFunc("POST /api/login", apiCfg.checkPassword)
	mux.HandleFunc("POST /api/refresh", apiCfg.refreshHandler)
	mux.HandleFunc("POST /api/revoke", apiCfg.revokeHandler)
	mux.HandleFunc("PUT /api/users", apiCfg.updateUser)
	mux.HandleFunc("DELETE /api/chirps/{chirpID}", apiCfg.handlerChirpsDelete)
	fileServer := http.FileServer(http.Dir("."))
	handler := http.StripPrefix("/app", fileServer)
	mux.Handle("/app/", apiCfg.middlewareMetricsInc(handler))

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	server.ListenAndServe()
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

type apiConfig struct {
	DB             *database.Queries
	Platform       string
	fileserverHits atomic.Int32
	jwtSecret      string
}

func (cfg *apiConfig) middlewareMetricsInc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg.fileserverHits.Add(1)
		next.ServeHTTP(w, r)
	})
}

func (cfg *apiConfig) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	html := fmt.Sprintf("<html>\n  <body>\n    <h1>Welcome, Chirpy Admin</h1>\n    <p>Chirpy has been visited %d times!</p>\n  </body>\n</html>", cfg.fileserverHits.Load())
	w.Write([]byte(html))
}

func (cfg *apiConfig) resetHandler(w http.ResponseWriter, r *http.Request) {
	if cfg.Platform != "dev" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	err := cfg.DB.DeleteAllUsers(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	cfg.fileserverHits.Store(0)
	w.WriteHeader(http.StatusOK)
}

type Chirp struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Body      string    `json:"body"`
	UserID    uuid.UUID `json:"user_id"`
}

func (cfg *apiConfig) newChirp(w http.ResponseWriter, r *http.Request) {
	type parameters struct {
		Body string `json:"body"`
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		http.Error(w, "Couldn't find JWT: "+err.Error(), http.StatusUnauthorized)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		http.Error(w, "Couldn't validate JWT "+err.Error(), http.StatusUnauthorized)
		return
	}
	now := time.Now().UTC()
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	params := parameters{}
	err = decoder.Decode(&params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(params.Body) > 140 {
		response := map[string]string{
			"error": "Chirp is too long",
		}
		jsonResponse, err := json.Marshal(response)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(jsonResponse)
		return
	}
	params.Body = cleanChirp(params.Body)
	dbChirp, err := cfg.DB.CreateChirp(r.Context(), database.CreateChirpParams{
		ID:        uuid.New(),
		CreatedAt: now,
		UpdatedAt: now,
		Body:      params.Body,
		UserID:    userID,
	})
	if err != nil {
		errorResponse := map[string]string{
			"error": "Internal server error",
		}
		jsonResponse, err := json.Marshal(errorResponse)
		if err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(jsonResponse)
		return
	}
	responseChirp := Chirp{
		ID:        dbChirp.ID,
		CreatedAt: dbChirp.CreatedAt,
		UpdatedAt: dbChirp.UpdatedAt,
		Body:      dbChirp.Body,
		UserID:    dbChirp.UserID,
	}
	jsonResponse, err := json.Marshal(responseChirp)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(jsonResponse)
}

func cleanChirp(body string) string {
	profanity := []string{"kerfuffle", "sharbert", "fornax"}
	words := strings.Split(body, " ")
	for i, word := range words {
		lowercaseWord := strings.ToLower(word)
		for _, p := range profanity {
			if lowercaseWord == p {
				words[i] = "****"
				break
			}
		}
	}
	return strings.Join(words, " ")
}

type User struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Email     string    `json:"email"`
}

func (cfg *apiConfig) handleNewUser(w http.ResponseWriter, r *http.Request) {
	type parameters struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	params := parameters{}
	err := decoder.Decode(&params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(params.Email) == 0 {
		http.Error(w, "Email is required", http.StatusBadRequest)
		return
	}
	if len(params.Password) == 0 {
		http.Error(w, "Password is required", http.StatusBadRequest)
		return
	}
	hashedPassword, err := auth.HashPassword(params.Password)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	user, err := cfg.DB.CreateUser(r.Context(), database.CreateUserParams{
		Email:          params.Email,
		HashedPassword: hashedPassword,
	})
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	responseUser := User{
		ID:        user.ID,
		CreatedAt: user.CreatedAt,
		UpdatedAt: user.UpdatedAt,
		Email:     user.Email,
	}
	jsonResponse, err := json.Marshal(responseUser)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(jsonResponse)
}

func (cfg *apiConfig) getAllChirps(w http.ResponseWriter, r *http.Request) {
	chirps, err := cfg.DB.GetAllChirps(r.Context())
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	responseChirps := make([]Chirp, len(chirps))
	for i, chirp := range chirps {
		responseChirps[i] = Chirp{
			ID:        chirp.ID,
			CreatedAt: chirp.CreatedAt,
			UpdatedAt: chirp.UpdatedAt,
			Body:      chirp.Body,
			UserID:    chirp.UserID,
		}
	}
	jsonResponse, err := json.Marshal(responseChirps)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonResponse)
}

func (cfg *apiConfig) getOneChirp(w http.ResponseWriter, r *http.Request) {
	basePath := "/api/chirps/"
	if !strings.HasPrefix(r.URL.Path, basePath) {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}
	chirpID := strings.TrimPrefix(r.URL.Path, basePath)
	if chirpID == "" {
		http.Error(w, "Chirp ID is required", http.StatusBadRequest)
		return
	}
	chirpUUID, err := uuid.Parse(chirpID)
	if err != nil {
		http.Error(w, "Invalid chirp ID format", http.StatusBadRequest)
		return
	}
	chirp, err := cfg.DB.GetChirpByID(r.Context(), chirpUUID)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Chirp not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	responseChirp := Chirp{
		ID:        chirp.ID,
		CreatedAt: chirp.CreatedAt,
		UpdatedAt: chirp.UpdatedAt,
		Body:      chirp.Body,
		UserID:    chirp.UserID,
	}
	jsonResponse, err := json.Marshal(responseChirp)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonResponse)
}

func (cfg *apiConfig) handlerChirpsDelete(w http.ResponseWriter, r *http.Request) {
	chirpIDString := r.PathValue("chirpID")
	chirpID, err := uuid.Parse(chirpIDString)
	if err != nil {
		http.Error(w, "Invalid chirp ID format", http.StatusBadRequest)
		return
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		http.Error(w, "Couldn't find JWT", http.StatusUnauthorized)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		http.Error(w, "Couldn't find JWT", http.StatusUnauthorized)
		return
	}
	dbChirp, err := cfg.DB.GetChirp(r.Context(), chirpID)
	if err != nil {
		http.Error(w, "Couldn't get chirp", http.StatusNotFound)
		return
	}
	if dbChirp.UserID != userID {
		http.Error(w, "You can't delete this chirp", http.StatusForbidden)
		return
	}
	err = cfg.DB.DeleteChirp(r.Context(), chirpID)
	if err != nil {
		http.Error(w, "Couldn't delete chirp", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (cfg *apiConfig) checkPassword(w http.ResponseWriter, r *http.Request) {
	type parameters struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	params := parameters{}
	err := decoder.Decode(&params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(params.Email) == 0 {
		http.Error(w, "Email is required", http.StatusBadRequest)
		return
	}
	if len(params.Password) == 0 {
		http.Error(w, "Password is required", http.StatusBadRequest)
		return
	}
	user, err := cfg.DB.GetUserByEmail(r.Context(), params.Email)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "incorrect email or password", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	err = auth.CheckPasswordHash(user.HashedPassword, params.Password)
	if err != nil {
		http.Error(w, "Invalid email or password", http.StatusUnauthorized)
		return
	}
	type safeUser struct {
		ID           string    `json:"id"`
		Email        string    `json:"email"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
		Token        string    `json:"token"`
		RefreshToken string    `json:"refresh_token"`
	}
	expirationTime := time.Hour
	accessToken, err := auth.MakeJWT(
		user.ID,
		cfg.jwtSecret,
		expirationTime,
	)
	if err != nil {
		http.Error(w, "Couldn't create access JWT: "+err.Error(), http.StatusInternalServerError)
		return
	}
	refreshToken, err := auth.MakeRefreshToken()
	if err != nil {
		http.Error(w, "Couldn't create refresh token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	refreshTokenExpiration := time.Now().Add(60 * 24 * time.Hour)
	_, err = cfg.DB.CreateRefreshToken(r.Context(), database.CreateRefreshTokenParams{
		Token:     refreshToken,
		UserID:    user.ID,
		ExpiresAt: refreshTokenExpiration,
	})
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	safeUserResponse := safeUser{
		ID:           user.ID.String(),
		Email:        user.Email,
		CreatedAt:    user.CreatedAt,
		UpdatedAt:    user.UpdatedAt,
		Token:        accessToken,
		RefreshToken: refreshToken,
	}
	jsonResponse, err := json.Marshal(safeUserResponse)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonResponse)
}

func (cfg *apiConfig) refreshHandler(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		http.Error(w, "Authorization header is required", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "Invalid authorization header format", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	refreshToken, err := cfg.DB.GetRefreshToken(r.Context(), token)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Refresh token not found", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if time.Now().After(refreshToken.ExpiresAt) {
		http.Error(w, "Refresh token expired", http.StatusUnauthorized)
		return
	}
	if refreshToken.RevokedAt.Valid {
		http.Error(w, "Refresh token revoked", http.StatusUnauthorized)
		return
	}
	accessToken, err := auth.MakeJWT(refreshToken.UserID, cfg.jwtSecret, time.Hour)
	if err != nil {
		http.Error(w, "Couldn't create access token", http.StatusInternalServerError)
		return
	}
	responseData := map[string]string{"token": accessToken}
	responseJSON, err := json.Marshal(responseData)
	if err != nil {
		http.Error(w, "Failed to marshal JSON response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responseJSON)
}

func (cfg *apiConfig) revokeHandler(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		http.Error(w, "Authorization header is required", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "Invalid authorization header format", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	refreshToken, err := cfg.DB.GetRefreshToken(r.Context(), token)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Refresh token not found", http.StatusUnauthorized)
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if refreshToken.RevokedAt.Valid {
		http.Error(w, "Refresh token already revoked", http.StatusBadRequest)
		return
	}
	err = cfg.DB.RevokeRefreshToken(r.Context(), token)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (cfg *apiConfig) updateUser(w http.ResponseWriter, r *http.Request) {
	type parameters struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		http.Error(w, "Couldn't find JWT: "+err.Error(), http.StatusUnauthorized)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		http.Error(w, "Couldn't validate JWT "+err.Error(), http.StatusUnauthorized)
		return
	}
	decoder := json.NewDecoder(r.Body)
	defer r.Body.Close()
	params := parameters{}
	err = decoder.Decode(&params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var hashedPassword string
	if params.Password != "" {
		hash, err := auth.HashPassword(params.Password)
		if err != nil {
			http.Error(w, "Failed to hash password: "+err.Error(), http.StatusInternalServerError)
			return
		}
		hashedPassword = hash
	}
	if params.Email == "" {
		http.Error(w, "Email cannot be empty", http.StatusBadRequest)
		return
	}
	params2 := database.UpdateUserByIDParams{
		Email:          params.Email,
		HashedPassword: hashedPassword,
		ID:             userID,
	}
	err = cfg.DB.UpdateUserByID(r.Context(), params2)
	if err != nil {
		http.Error(w, "Failed to update user: "+err.Error(), http.StatusInternalServerError)
		return
	}
	updatedUser, err := cfg.DB.GetUserByID(r.Context(), userID)
	if err != nil {
		http.Error(w, "Failed to retrieve updated user: "+err.Error(), http.StatusInternalServerError)
		return
	}
	type userResponse struct {
		ID        string    `json:"id"`
		Email     string    `json:"email"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	response := userResponse{
		ID:        updatedUser.ID.String(),
		Email:     updatedUser.Email,
		CreatedAt: updatedUser.CreatedAt,
		UpdatedAt: updatedUser.UpdatedAt,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
}
