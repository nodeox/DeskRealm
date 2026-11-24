"""Verify real Authenticode signatures over both installer architectures."""

import os
import shutil
import subprocess
import unittest

import windows_package_test
import windows_installer as setup


class WindowsInstallerTests(unittest.TestCase):
    def setUp(self):
        self.authenticode = os.environ.get("DESKREALM_TEST_OSSLSIGNCODE") or shutil.which("osslsigncode")
        if not self.authenticode:
            if os.environ.get("DESKREALM_REQUIRE_OSSLSIGNCODE") == "1":
                self.fail("set DESKREALM_TEST_OSSLSIGNCODE or install osslsigncode")
            self.skipTest("osslsigncode is unavailable")

    def test_verified_installers_bind_both_architectures_payloads_and_certificate(self):
        fixture = windows_package_test.SignedPackageTests(methodName="test_deterministic_retry_and_separate_signer")
        fixture.setUp()
        self.addCleanup(fixture.doCleanups)
        fixture.prepare()
        fixture.sign()
        unsigned = fixture.root / "unsigned"
        unsigned.mkdir()
        signed = fixture.root / "signed"
        signed.mkdir()
        host, release_sha = setup.prepare(fixture.approval, fixture.destination, fixture.signature, fixture.public,
                                          unsigned, fixture.root, fixture.openssl)

        def openssl(*args):
            subprocess.run([fixture.openssl, *args], check=True, capture_output=True)

        ca_key = fixture.key_dir / "ca.key"
        ca = fixture.key_dir / "ca.pem"
        openssl("req", "-new", "-x509", "-newkey", "rsa:2048", "-noenc", "-days", "2",
                "-subj", "/CN=DeskRealm test CA", "-addext", "basicConstraints=critical,CA:TRUE",
                "-addext", "keyUsage=critical,keyCertSign,cRLSign", "-keyout", ca_key, "-out", ca)
        extensions = fixture.key_dir / "publisher.ext"
        extensions.write_text("basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n"
                              "extendedKeyUsage=critical,codeSigning\n", encoding="ascii")

        def certificate(label):
            private = fixture.key_dir / (label + ".key")
            request = fixture.key_dir / (label + ".csr")
            cert = fixture.key_dir / (label + ".pem")
            openssl("req", "-new", "-newkey", "rsa:2048", "-noenc", "-subj", f"/CN={label}",
                    "-keyout", private, "-out", request)
            openssl("x509", "-req", "-in", request, "-CA", ca, "-CAkey", ca_key, "-CAcreateserial",
                    "-days", "1", "-extfile", extensions, "-out", cert)
            return private, cert

        private, cert = certificate("DeskRealm development signer")
        _, alternate = certificate("Other valid signer under the same CA")
        for arch in setup.delivery.ARCH:
            name = setup.setup_name(arch)
            subprocess.run([self.authenticode, "sign", "-certs", str(cert), "-key", str(private), "-h", "sha256",
                            "-in", str(unsigned / name), "-out", str(signed / name)], check=True)
        evidence = setup.verify(signed, unsigned, host, ca, cert, self.authenticode, fixture.openssl)
        self.assertEqual(release_sha, setup.build.sha_file(fixture.destination / setup.delivery.MANIFEST))
        self.assertEqual(set(evidence["installers"]), set(setup.delivery.ARCH))
        with self.assertRaises(subprocess.CalledProcessError):
            setup.verify(signed, unsigned, host, ca, alternate, self.authenticode, fixture.openssl)
        x64 = signed / setup.setup_name("x64")
        arm64 = signed / setup.setup_name("arm64")
        x64.write_bytes(arm64.read_bytes())
        with self.assertRaises(subprocess.CalledProcessError):
            setup.verify(signed, unsigned, host, ca, cert, self.authenticode, fixture.openssl)
        weak = fixture.root / "weak"
        weak.mkdir()
        subprocess.run([self.authenticode, "sign", "-certs", str(cert), "-key", str(private), "-h", "sha1",
                        "-in", str(unsigned / x64.name), "-out", str(weak / x64.name)], check=True)
        shutil.copyfile(arm64, weak / arm64.name)
        with self.assertRaisesRegex(ValueError, "Authenticode and image digests must use SHA-256"):
            setup.verify(weak, unsigned, host, ca, cert, self.authenticode, fixture.openssl)


if __name__ == "__main__":
    unittest.main()
