// A seeded random property test of the verifier: mutated and made-up tokens
// are refused with EdgeAuthInvalid, and a token is accepted only when its
// signed part (header and payload) is a valid vector's. Set EDGEAUTH_SEED to
// replay a failure; the seed is in the failure message.
import { readFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createVerifier, EdgeAuthInvalid, HEADER } from "../src/index.ts";

interface Vectors {
  now: number;
  jwks: unknown;
  cases: {
    name: string;
    token: string;
    audiences: string[];
    want: { ok: boolean };
  }[];
}

const vectors: Vectors = JSON.parse(
  readFileSync(
    new URL("../../../edgeauth/testdata/vectors.json", import.meta.url),
    "utf8",
  ),
);
const AUDIENCE = "live.metsa.app";
const RUNS = 3000;
const SEED = Number(process.env.EDGEAUTH_SEED ?? 20261118);
const B64URL =
  "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

// mulberry32: small, seedable, good enough to pick mutations.
function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const signedPart = (token: string) => token.slice(0, token.lastIndexOf("."));
const b64 = (s: string) => Buffer.from(s).toString("base64url");

function mutate(token: string, random: () => number): string {
  const int = (n: number) => Math.floor(random() * n);
  const char = () => B64URL[int(B64URL.length)];
  const at = int(token.length);
  const [header, payload, signature] = token.split(".");
  switch (int(8)) {
    case 0: // replace one character
      return token.slice(0, at) + char() + token.slice(at + 1);
    case 1: // delete one character
      return token.slice(0, at) + token.slice(at + 1);
    case 2: // insert one character
      return token.slice(0, at) + char() + token.slice(at);
    case 3: // truncate
      return token.slice(0, at);
    case 4: // another alg, same payload and signature
      return [
        b64(
          JSON.stringify({
            alg: ["none", "HS256", "ES384", "RS256", "es256"][int(5)],
            typ: "bastion-id+jwt",
            kid: "vectors-2026-11-a",
          }),
        ),
        payload,
        int(2) ? signature : "",
      ].join(".");
    case 5: // swap the segments
      return [payload, header, signature].join(".");
    case 6: // another vector's signature
      return `${header}.${payload}.${
        vectors.cases[int(vectors.cases.length)]?.token.split(".")[2] ?? ""
      }`;
    default: // random base64url segments
      return Array.from({ length: 1 + int(4) }, () =>
        Array.from({ length: int(64) }, char).join(""),
      ).join(".");
  }
}

let server: Server;
let jwksUrl: string;

beforeAll(async () => {
  server = createServer((_req, res) => {
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(vectors.jwks));
  });
  jwksUrl = await new Promise<string>((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolve(`http://127.0.0.1:${port}/jwks.json`);
    });
  });
});

afterAll(async () => {
  await new Promise((resolve) => server.close(resolve));
});

describe("random tokens", () => {
  const valid = vectors.cases.filter(
    (c) => c.want.ok && c.audiences.length === 1 && c.audiences[0] === AUDIENCE,
  );
  const signed = new Set(valid.map((c) => signedPart(c.token)));

  it(`refuses mutations (seed ${SEED}, ${RUNS} runs)`, async () => {
    expect(valid.length).toBeGreaterThan(0);
    const v = createVerifier({
      jwksUrl,
      audiences: [AUDIENCE],
      now: () => new Date(vectors.now * 1000),
    });
    const random = rng(SEED);
    for (let i = 0; i < RUNS; i++) {
      const base = vectors.cases[Math.floor(random() * vectors.cases.length)];
      if (!base) {
        continue;
      }
      const token = mutate(base.token, random);
      try {
        const id = await v.verify(token);
        expect(
          signed.has(signedPart(token)),
          `seed ${SEED} run ${i}: accepted ${token}`,
        ).toBe(true);
        expect(id.subject).not.toBe("");
      } catch (err) {
        expect(err, `seed ${SEED} run ${i}: ${token}`).toBeInstanceOf(
          EdgeAuthInvalid,
        );
      }
    }
  });

  it("refuses two identity headers", async () => {
    const v = createVerifier({
      jwksUrl,
      audiences: [AUDIENCE],
      now: () => new Date(vectors.now * 1000),
    });
    const token = valid[0]?.token ?? "";
    const headers = new Headers();
    headers.append(HEADER, token);
    headers.append(HEADER, token);
    await expect(v.fromHeaders(headers)).rejects.toBeInstanceOf(
      EdgeAuthInvalid,
    );
  });
});
