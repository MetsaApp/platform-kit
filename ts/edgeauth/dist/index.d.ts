/** The header guard sends the identity JWT in. */
export declare const HEADER = "x-bastion-identity";
/** The iss of every identity JWT. */
export declare const ISSUER = "bastion";
/** The typ in the JOSE header of every identity JWT. */
export declare const TOKEN_TYPE = "bastion-id+jwt";
/** Guard's in-cluster JWKS. */
export declare const DEFAULT_JWKS_URL = "http://guard.guard-production.svc.cluster.local:8082/jwks.json";
/** Clock skew tolerated on exp, iat and nbf, in seconds. */
export declare const LEEWAY_SECONDS = 10;
/** The caller guard vouches for. */
export interface Identity {
    /** The Zitadel user id. */
    subject: string;
    email: string;
    emailVerified: boolean;
    name: string;
    /** Role keys of the host's Zitadel project; empty when none. */
    roles: string[];
    /** Opaque session id; empty for a bearer. */
    sessionId: string;
    /** When the user logged in at Zitadel; null when unknown. */
    authTime: Date | null;
    /** The Zitadel client the session or bearer belongs to. */
    clientId: string;
    /** "session" or "bearer". */
    source: string;
}
/** The token is present but not acceptable. Answer 401. */
export declare class EdgeAuthInvalid extends Error {
    constructor(message: string, options?: ErrorOptions);
}
/** The token could not be checked because guard's JWKS is unreachable. Answer 503. */
export declare class EdgeAuthUnavailable extends Error {
    constructor(message: string, options?: ErrorOptions);
}
export interface VerifierOptions {
    /** Guard's JWKS. Default PLATFORM_KIT_EDGEAUTH_JWKS_URL, else the in-cluster endpoint. */
    jwksUrl?: string;
    /** The hosts this app serves (lower case, no port). Required. */
    audiences: string[];
    /** The clock for the token's time checks, for tests. */
    now?: () => Date;
}
export interface Verifier {
    /** Checks a token; rejects with EdgeAuthInvalid or EdgeAuthUnavailable. */
    verify(token: string): Promise<Identity>;
    /** Verifies the identity header; null when the request has none (anonymous). */
    fromHeaders(headers: Headers): Promise<Identity | null>;
}
/** Builds a verifier. It never contacts guard until the first token. */
export declare function createVerifier(options: VerifierOptions): Verifier;
