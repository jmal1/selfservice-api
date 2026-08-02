-- Persist the raw OIDC id_token per server-side session.
--
-- On logout we need to send the id_token back to Authentik's
-- end_session_endpoint as the `id_token_hint` so the IdP knows which SSO
-- session to terminate. Without it, Authentik keeps the SSO session alive
-- and the user is silently re-authenticated on the next /auth/login.
--
-- Stored on user_sessions (not in the signed JWT cookie) so the raw token
-- never leaves the server and is cleaned up when the session row is.
ALTER TABLE user_sessions
    ADD COLUMN IF NOT EXISTS id_token TEXT;
