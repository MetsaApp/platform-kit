import { readFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import {
  createVerifier,
  EdgeAuthInvalid,
  EdgeAuthUnavailable,
  HEADER,
  type Identity,
} from "../src/index.ts";

interface Want {
  ok: boolean;
  error?: "invalid" | "unavailable";
  subject?: string;
  email?: string;
  emailVerified?: boolean;
  name?: string;
  roles?: string[];
  sessionId?: string;
  authTime?: number;
  clientId?: string;
  source?: string;
}

interface Vectors {
  now: number;
  jwks: unknown;
  cases: {
    name: string;
    token: string;
    audiences: string[];
    jwksUnreachable?: boolean;
    want: Want;
  }[];
}

const vectors: Vectors = JSON.parse(
  readFileSync(
    new URL("../../../edgeauth/testdata/vectors.json", import.meta.url),
    "utf8",
  ),
);
const now = () => new Date(vectors.now * 1000);

let server: Server;
let jwksUrl: string;
let fetches = 0;
// A port nothing listens on: a server bound and closed again.
let downUrl: string;

function listen(s: Server): Promise<string> {
  return new Promise((resolve) => {
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address() as AddressInfo;
      resolve(`http://127.0.0.1:${port}/jwks.json`);
    });
  });
}

beforeAll(async () => {
  server = createServer((_req, res) => {
    fetches++;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(vectors.jwks));
  });
  jwksUrl = await listen(server);
  const closed = createServer();
  downUrl = await listen(closed);
  await new Promise((resolve) => closed.close(resolve));
});

afterAll(async () => {
  await new Promise((resolve) => server.close(resolve));
});

function wantIdentity(w: Want): Identity {
  return {
    subject: w.subject ?? "",
    email: w.email ?? "",
    emailVerified: w.emailVerified ?? false,
    name: w.name ?? "",
    roles: w.roles ?? [],
    sessionId: w.sessionId ?? "",
    authTime: w.authTime ? new Date(w.authTime * 1000) : null,
    clientId: w.clientId ?? "",
    source: w.source ?? "",
  };
}

describe("conformance vectors", () => {
  it("has cases", () => {
    expect(vectors.cases.length).toBeGreaterThan(20);
  });

  for (const c of vectors.cases) {
    it(c.name, async () => {
      const v = createVerifier({
        jwksUrl: c.jwksUnreachable ? downUrl : jwksUrl,
        audiences: c.audiences,
        now,
      });
      const result = v.verify(c.token);
      if (c.want.error === "invalid") {
        await expect(result).rejects.toBeInstanceOf(EdgeAuthInvalid);
      } else if (c.want.error === "unavailable") {
        await expect(result).rejects.toBeInstanceOf(EdgeAuthUnavailable);
      } else {
        expect(await result).toEqual(wantIdentity(c.want));
      }
    });
  }
});

describe("verifier", () => {
  const valid = vectors.cases.find((c) => c.name === "valid session");
  if (!valid) {
    throw new Error("no valid session vector");
  }

  it("needs audiences", () => {
    expect(() => createVerifier({ audiences: [] })).toThrow(/audiences/);
  });

  it("does not fetch at construction and caches the JWKS", async () => {
    const before = fetches;
    const v = createVerifier({ jwksUrl, audiences: valid.audiences, now });
    expect(fetches).toBe(before);
    await v.verify(valid.token);
    await v.verify(valid.token);
    expect(fetches).toBe(before + 1);
  });

  it("treats a request without the header as anonymous", async () => {
    const v = createVerifier({ jwksUrl, audiences: valid.audiences, now });
    expect(await v.fromHeaders(new Headers())).toBeNull();
    const id = await v.fromHeaders(new Headers({ [HEADER]: valid.token }));
    expect(id?.subject).toBe(valid.want.subject);
    await expect(
      v.fromHeaders(new Headers({ [HEADER]: "garbage" })),
    ).rejects.toBeInstanceOf(EdgeAuthInvalid);
  });

  it("never puts the token in an error message", async () => {
    const v = createVerifier({ jwksUrl, audiences: ["other.metsa.app"], now });
    await expect(v.verify(valid.token)).rejects.toSatisfy(
      (err: Error) => !err.message.includes(valid.token),
    );
  });
});
