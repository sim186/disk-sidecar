/*
 * SHA-256 for the C port. On macOS it is CommonCrypto (the system library, which uses the ARMv8
 * SHA-2 instructions, like Go's crypto/sha256 and Rust's sha2 crate do). Elsewhere, or with
 * -DSIDECAR_PORTABLE_SHA256, it is the plain C implementation in sha256.c.
 */
#ifndef SIDECAR_SHA256_H
#define SIDECAR_SHA256_H
#include <stddef.h>
#include <stdint.h>

#if defined(__APPLE__) && !defined(SIDECAR_PORTABLE_SHA256)
#include <CommonCrypto/CommonDigest.h>
typedef CC_SHA256_CTX sha256_ctx;
static inline void sha256_init(sha256_ctx *c) { CC_SHA256_Init(c); }
static inline void sha256_update(sha256_ctx *c, const void *p, size_t n) {
    const unsigned char *b = p;
    while (n > 0) { /* CC_LONG is 32 bits */
        CC_LONG k = n > 0x40000000u ? 0x40000000u : (CC_LONG)n;
        CC_SHA256_Update(c, b, k);
        b += k, n -= k;
    }
}
static inline void sha256_final(sha256_ctx *c, unsigned char out[32]) { CC_SHA256_Final(out, c); }
#else
typedef struct {
    uint32_t h[8];
    uint64_t len;
    unsigned char block[64];
    size_t fill;
} sha256_ctx;
void sha256_init(sha256_ctx *c);
void sha256_update(sha256_ctx *c, const void *p, size_t n);
void sha256_final(sha256_ctx *c, unsigned char out[32]);
#endif
#endif
