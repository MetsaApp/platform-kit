/**
 * Verifies the identity JWT guard hands every authenticated request upstream
 * in the x-bastion-identity header. The same rules as the Go package
 * github.com/MetsaApp/platform-kit/edgeauth, checked by the same conformance
 * vectors (edgeauth/testdata/vectors.json):
 *
 * - alg ES256 and typ bastion-id+jwt, with a kid in guard's JWKS;
 * - iss "bastion", aud one of the configured audiences;
 * - exp not passed and iat not in the future, with 10 s leeway, and sub set.
 *
 * The JWKS is fetched on the first token, cached for 5 minutes, and refetched
 * for an unknown kid at most once every 30 seconds.
 */
import { createRemoteJWKSet, errors, jwtVerify, } from "jose";
/** The header guard sends the identity JWT in. */
export const HEADER = "x-bastion-identity";
/** The iss of every identity JWT. */
export const ISSUER = "bastion";
/** The typ in the JOSE header of every identity JWT. */
export const TOKEN_TYPE = "bastion-id+jwt";
/** Guard's in-cluster JWKS. */
export const DEFAULT_JWKS_URL = "http://guard.guard-production.svc.cluster.local:8082/jwks.json";
/** Clock skew tolerated on exp, iat and nbf, in seconds. */
export const LEEWAY_SECONDS = 10;
const CACHE_MAX_AGE_MS = 5 * 60 * 1000;
const COOLDOWN_MS = 30 * 1000;
const FETCH_TIMEOUT_MS = 5 * 1000;
/** The token is present but not acceptable. Answer 401. */
export class EdgeAuthInvalid extends Error {
    constructor(message, options) {
        super(`edgeauth: invalid identity token: ${message}`, options);
        this.name = "EdgeAuthInvalid";
    }
}
/** The token could not be checked because guard's JWKS is unreachable. Answer 503. */
export class EdgeAuthUnavailable extends Error {
    constructor(message, options) {
        super(`edgeauth: identity keys unavailable: ${message}`, options);
        this.name = "EdgeAuthUnavailable";
    }
}
function envJwksUrl() {
    const env = globalThis.process?.env;
    return env?.PLATFORM_KIT_EDGEAUTH_JWKS_URL || undefined;
}
/** Same comparison as jose and RFC 7515: case-insensitive, "application/" optional. */
function normalizeType(typ) {
    const lower = typ.toLowerCase();
    return lower.includes("/") ? lower : `application/${lower}`;
}
function optionalString(payload, claim) {
    const value = payload[claim];
    if (value === undefined || value === null) {
        return "";
    }
    if (typeof value !== "string") {
        throw new EdgeAuthInvalid(`${claim} is not a string`);
    }
    return value;
}
function identityOf(payload) {
    const verified = payload.email_verified;
    if (verified !== undefined &&
        verified !== null &&
        typeof verified !== "boolean") {
        throw new EdgeAuthInvalid("email_verified is not a boolean");
    }
    const roles = payload.roles ?? [];
    if (!Array.isArray(roles) ||
        !roles.every((r) => typeof r === "string")) {
        throw new EdgeAuthInvalid("roles is not a list of strings");
    }
    const authTime = payload.auth_time;
    if (authTime !== undefined &&
        authTime !== null &&
        typeof authTime !== "number") {
        throw new EdgeAuthInvalid("auth_time is not a number");
    }
    return {
        subject: optionalString(payload, "sub"),
        email: optionalString(payload, "email"),
        emailVerified: verified === true,
        name: optionalString(payload, "name"),
        roles: [...roles],
        sessionId: optionalString(payload, "sid"),
        authTime: typeof authTime === "number" && authTime > 0
            ? new Date(Math.trunc(authTime) * 1000)
            : null,
        clientId: optionalString(payload, "azp"),
        source: optionalString(payload, "src"),
    };
}
/** Builds a verifier. It never contacts guard until the first token. */
export function createVerifier(options) {
    const audiences = options.audiences
        .map((a) => a.trim().toLowerCase())
        .filter((a) => a !== "");
    if (audiences.length === 0) {
        throw new Error("edgeauth: no audiences: set the hosts this app serves");
    }
    const jwksUrl = options.jwksUrl ?? envJwksUrl() ?? DEFAULT_JWKS_URL;
    const now = options.now ?? (() => new Date());
    const remote = createRemoteJWKSet(new URL(jwksUrl), {
        cacheMaxAge: CACHE_MAX_AGE_MS,
        cooldownDuration: COOLDOWN_MS,
        timeoutDuration: FETCH_TIMEOUT_MS,
    });
    // Header checks come before the key lookup, as in the Go verifier, so a
    // malformed token never costs a JWKS fetch; fetch failures are told apart
    // from an unknown key.
    const getKey = async (header, token) => {
        if (typeof header.typ !== "string" ||
            normalizeType(header.typ) !== normalizeType(TOKEN_TYPE)) {
            throw new EdgeAuthInvalid(`typ is not ${TOKEN_TYPE}`);
        }
        if (typeof header.kid !== "string" || header.kid === "") {
            throw new EdgeAuthInvalid("no kid");
        }
        try {
            return await remote(header, token);
        }
        catch (err) {
            if (err instanceof errors.JWKSNoMatchingKey ||
                err instanceof errors.JWKSMultipleMatchingKeys) {
                throw new EdgeAuthInvalid("unknown kid", { cause: err });
            }
            throw new EdgeAuthUnavailable("JWKS fetch failed", { cause: err });
        }
    };
    async function verify(token) {
        if (token === "") {
            throw new EdgeAuthInvalid("empty");
        }
        const currentDate = now();
        let payload;
        try {
            ({ payload } = await jwtVerify(token, getKey, {
                algorithms: ["ES256"],
                typ: TOKEN_TYPE,
                issuer: ISSUER,
                audience: audiences,
                clockTolerance: LEEWAY_SECONDS,
                currentDate,
                requiredClaims: ["sub", "exp", "iat"],
            }));
        }
        catch (err) {
            if (err instanceof EdgeAuthInvalid || err instanceof EdgeAuthUnavailable) {
                throw err;
            }
            const reason = err instanceof errors.JOSEError ? err.code : "malformed";
            throw new EdgeAuthInvalid(reason, { cause: err });
        }
        // jose checks iat only with maxTokenAge; the contract refuses a token
        // issued in the future.
        const nowSeconds = Math.floor(currentDate.getTime() / 1000);
        if (typeof payload.iat !== "number" ||
            payload.iat > nowSeconds + LEEWAY_SECONDS) {
            throw new EdgeAuthInvalid("issued in the future");
        }
        if (typeof payload.sub !== "string" || payload.sub === "") {
            throw new EdgeAuthInvalid("no sub");
        }
        return identityOf(payload);
    }
    return {
        verify,
        async fromHeaders(headers) {
            const token = headers.get(HEADER);
            if (token === null || token === "") {
                return null;
            }
            return verify(token);
        },
    };
}
