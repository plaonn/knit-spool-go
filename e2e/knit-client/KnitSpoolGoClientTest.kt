// Copied into a getknit/knit checkout by scripts/knit-client-e2e.sh and compiled with its unit tests, so it
// runs the released Knit client's own link parser, dialer and connection state machine against a running
// knit-spool-go. It is not part of this repository's Go build.
package app.getknit.knit.mesh.spool

import app.getknit.knit.mesh.crypto.scope.ScopeCrypto
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import okhttp3.OkHttpClient
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.net.URI
import java.security.KeyStore
import java.security.MessageDigest
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

/**
 * A newcomer's path through a knit-spool-go relay invite link: the link parses, its URL dials with the
 * token it carries, the HELLO lists the link's room, and two members exchange a frame in that room.
 */
class KnitSpoolGoClientTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    @After
    fun stop() = scope.cancel()

    private class Member(
        val conn: SpoolConnection,
        val socket: SpoolSocket,
        val digests: Channel<SpoolDigest>,
        val events: Channel<SpoolEvent>,
        val errors: Channel<String>,
    )

    private fun env(name: String): String = System.getenv(name) ?: error("$name is not set; run scripts/knit-client-e2e.sh")

    private fun trustingClient(caFile: File): OkHttpClient {
        val cert = caFile.inputStream().use { CertificateFactory.getInstance("X.509").generateCertificate(it) as X509Certificate }
        val keys = KeyStore.getInstance(KeyStore.getDefaultType()).apply {
            load(null)
            setCertificateEntry("knit-spool-go", cert)
        }
        val trust = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm()).apply { init(keys) }
            .trustManagers.single() as X509TrustManager
        val tls = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trust), null) }
        return OkHttpSpoolDialer.defaultClient().newBuilder().sslSocketFactory(tls.socketFactory, trust).build()
    }

    private suspend fun join(
        dialer: OkHttpSpoolDialer,
        url: String,
    ): Member {
        val socket = dialer.dial(url) ?: error("the Knit dialer refused ${SpoolUrl.redact(url)}")
        val digests = Channel<SpoolDigest>(Channel.UNLIMITED)
        val events = Channel<SpoolEvent>(Channel.UNLIMITED)
        val errors = Channel<String>(Channel.UNLIMITED)
        val conn =
            SpoolConnection(
                url = url,
                link = socket,
                onDigest = { digests.send(it) },
                onEvent = { events.send(it) },
                onScopeError = { scopeHex, code, _ -> errors.send("$code for ${scopeHex ?: "the connection"}") },
            )
        scope.launch {
            for (bytes in socket.incoming) conn.onMessage(bytes)
            conn.onClosed()
        }
        val ready = withTimeout(WAIT_MS) { conn.awaitReady() }
        assertTrue("HELLO exchange failed: ${socket.closeReason ?: conn.fault}", ready)
        return Member(conn, socket, digests, events, errors)
    }

    private suspend fun Member.awaitDigest(scopeId: ByteArray) {
        withTimeout(WAIT_MS) {
            while (true) {
                errors.tryReceive().getOrNull()?.let { fail("SUB refused: $it") }
                val digest = digests.receive()
                if (digest.scope.contentEquals(scopeId)) return@withTimeout
            }
        }
    }

    @Test
    fun relayInviteLinkJoinsTheCommonsRoom() =
        runBlocking {
            val link = env("KNIT_SPOOL_GO_LINK")
            val expectedName = env("KNIT_SPOOL_GO_NAME")
            val client = trustingClient(File(env("KNIT_SPOOL_GO_CA")))

            // The release build's policy: no cleartext.
            val parsed = RelayInvite.parse(link, allowCleartext = false)
            val invite = parsed as? RelayInvite.Parsed.Invite ?: error("Knit rejected the link: $parsed")
            assertTrue(SpoolUrl.isAcceptable(invite.url, allowCleartext = false))
            assertEquals(expectedName, invite.name)
            assertNull("knit-spool-go never writes ticket fields", invite.key)
            val secret = checkNotNull(invite.secret) { "the link carries no room" }
            val room = ScopeCrypto.commonsScopeId(secret)

            val dialer = OkHttpSpoolDialer(allowCleartext = false, client = client)

            // Without the link's token the spool closes 4001 before any HELLO.
            val bare = URI(invite.url).let { URI(it.scheme, it.authority, it.path, null, null).toString() }
            val refused = dialer.dial(bare) ?: error("the Knit dialer refused $bare")
            val first = withTimeout(WAIT_MS) { refused.incoming.receiveCatching() }
            assertTrue("a tokenless client received a record", first.isClosed)
            assertTrue("tokenless close: ${refused.closeReason}", refused.closeReason?.startsWith("close 4001") == true)

            val alice = join(dialer, invite.url)
            val bob = join(dialer, invite.url)

            val advert = checkNotNull(alice.conn.adverts) { "HELLO advertises no room" }.advertForScope(room)
            assertTrue("HELLO does not list the link's room: $advert", advert is CommonsAdvert.Listed)
            val info = advert.info!!
            assertEquals(expectedName, info.name)

            val sub = listOf(ScopeSub(room, ScopeBounds(info.maxFrames, info.ttlMs, info.maxBlob)))
            assertTrue(alice.conn.sub(sub))
            alice.awaitDigest(room)
            assertTrue(bob.conn.sub(sub))
            bob.awaitDigest(room)

            val frame = "hello from the Knit client e2e".toByteArray()
            val blobId = MessageDigest.getInstance("SHA-256").digest(frame)
            val reply = withTimeout(WAIT_MS) { bob.conn.push(room, blobId, frame) }
            assertTrue("push reply: $reply", reply is SpoolReply.Ok)

            val event = withTimeout(WAIT_MS) { alice.events.receive() }
            assertArrayEquals(room, event.scope)
            assertArrayEquals(blobId, event.blobId)
            assertArrayEquals(frame, event.data)

            alice.socket.close(1000, "done")
            bob.socket.close(1000, "done")
        }

    private companion object {
        const val WAIT_MS = 15_000L
    }
}
