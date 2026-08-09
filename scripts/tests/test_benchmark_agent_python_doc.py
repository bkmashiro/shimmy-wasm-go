from __future__ import annotations

import io
import os
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
LAUNCHER = ROOT / "scripts" / "benchmark-agent-python-doc.sh"
PREPARER = ROOT / "scripts" / "prepare-agent-python-doc-bundle.sh"
JOB_SCRIPT = ROOT / "experiments" / "agent-python-ultimate" / "slurm" / "job.sh"
SAFE_EXTRACT = ROOT / "experiments" / "agent-python-ultimate" / "scripts" / "safe_extract_tar_zst.py"
VERIFY_BUNDLE = ROOT / "experiments" / "agent-python-ultimate" / "scripts" / "verify_doc_bundle.py"


class AgentPythonDoCLauncherTests(unittest.TestCase):
    def test_safe_extract_rejects_normalized_duplicate_members(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            temp = pathlib.Path(temporary)
            tar_path = temp / "duplicate.tar"
            with tarfile.open(tar_path, "w") as bundle:
                for name, payload in (("output/run/a", b"first"), ("output/run/./a", b"second")):
                    info = tarfile.TarInfo(name)
                    info.size = len(payload)
                    bundle.addfile(info, io.BytesIO(payload))
            archive = temp / "duplicate.tar.zst"
            subprocess.run(["zstd", "-q", "-f", str(tar_path), "-o", str(archive)], check=True)
            result = subprocess.run(
                [
                    str(SAFE_EXTRACT), str(archive), str(temp / "extract"),
                    "--max-compressed-bytes", "1048576", "--max-members", "10",
                    "--max-total-bytes", "1048576", "--max-file-bytes", "1048576",
                    "--required-root", "output",
                ],
                text=True, capture_output=True, check=False,
            )
            self.assertEqual(result.returncode, 2)
            self.assertIn("non-canonical archive member", result.stderr)

    def test_safe_extract_rejects_existing_destination_symlink(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            temp = pathlib.Path(temporary)
            tar_path = temp / "valid.tar"
            with tarfile.open(tar_path, "w") as bundle:
                payload = b"must-not-escape"
                info = tarfile.TarInfo("output/run/file")
                info.size = len(payload)
                bundle.addfile(info, io.BytesIO(payload))
            archive = temp / "valid.tar.zst"
            subprocess.run(["zstd", "-q", "-f", str(tar_path), "-o", str(archive)], check=True)
            victim = temp / "victim"
            victim.mkdir()
            destination = temp / "extract"
            destination.symlink_to(victim, target_is_directory=True)
            result = subprocess.run(
                [
                    str(SAFE_EXTRACT), str(archive), str(destination),
                    "--max-compressed-bytes", "1048576", "--max-members", "10",
                    "--max-total-bytes", "1048576", "--max-file-bytes", "1048576",
                    "--required-root", "output",
                ],
                text=True, capture_output=True, check=False,
            )
            self.assertEqual(result.returncode, 2)
            self.assertIn("destination must not already exist", result.stderr)
            self.assertEqual(list(victim.iterdir()), [])

            destination.unlink()
            result = subprocess.run(
                [
                    str(SAFE_EXTRACT), str(archive), str(destination),
                    "--max-compressed-bytes", "1048576", "--max-members", "10",
                    "--max-total-bytes", "1048576", "--max-file-bytes", "1048576",
                    "--required-root", "output",
                ],
                text=True, capture_output=True, check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((destination / "output" / "run" / "file").read_bytes(), payload)

    def run_launcher(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(LAUNCHER), *args],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_validate_run_id_rejects_path_like_values(self) -> None:
        for value in ("", "/tmp", "../escape", "a/b", "UPPERCASE", "short"):
            with self.subTest(value=value):
                result = self.run_launcher("validate-run-id", value)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("invalid run id", result.stderr)

    def test_validate_run_id_accepts_generated_shape(self) -> None:
        result = self.run_launcher(
            "validate-run-id", "agent-python-20260727t001500z-a1b2c3d4"
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_render_sbatch_is_manual_fixed_host_and_bounded(self) -> None:
        result = self.run_launcher(
            "render-sbatch", "agent-python-20260727t001500z-a1b2c3d4"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = result.stdout
        for required in (
            "--partition=a16",
            "--nodelist=gpuvm36",
            "--nodes=1",
            "--ntasks=1",
            "--cpus-per-task=6",
            "--mem=48G",
            "--gres=gpu:nvidia_a16:1",
            "--time=2-12:00:00",
            "--export=NIL",
            "--chdir=/tmp",
            "--output=/tmp/shimmy-agent-python-%j-slurm.out",
        ):
            self.assertIn(required, rendered)
        self.assertNotIn("--exclusive", rendered)
        self.assertNotIn(".github", rendered)
        launcher_text = LAUNCHER.read_text(encoding="utf-8")
        self.assertIn('sbcast -v --force --jobid="$job_id.batch"', launcher_text)
        self.assertIn("sbcast did not confirm the batch step credential", launcher_text)
        stage_body = launcher_text.split("stage_job() {", 1)[1].split("job_status() {", 1)[0]
        self.assertNotIn('test -d "/tmp/shimmy-agent-python-$job_id"', stage_body)
        self.assertIn("sleep 5\nfor name in input.tar.zst", stage_body)

    def test_render_canary_sbatch_uses_available_t4_capacity_with_two_hour_bound(self) -> None:
        result = self.run_launcher(
            "render-canary-sbatch", "agent-python-20260727t001500z-a1b2c3d4"
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = result.stdout
        self.assertIn("--partition=t4", rendered)
        self.assertIn("--nodelist=kingfisher", rendered)
        self.assertIn("--cpus-per-task=6", rendered)
        self.assertIn("--mem=48G", rendered)
        self.assertIn("--time=02:00:00", rendered)
        self.assertIn("--gres=gpu:tesla_t4:1", rendered)
        self.assertNotIn("--partition=a16", rendered)
        self.assertNotIn("--time=2-12:00:00", rendered)

    def test_slurm_job_uses_tmp_checksum_pull_ack_protocol(self) -> None:
        text = JOB_SCRIPT.read_text(encoding="utf-8")
        for required in (
            "umask 077",
            "SLURM_JOB_ID",
            "input.tar.zst",
            "sha256sum -c",
            "RESULT.READY",
            "ACK",
            'rm -rf -- "$run_root"',
        ):
            self.assertIn(required, text)
        self.assertIn('/tmp/shimmy-agent-python-', text)
        self.assertIn("input checksum file must contain exactly", text)
        self.assertIn('$checksum_name" != "input.tar.zst', text)
        self.assertIn("trap cleanup_run_root EXIT", text)
        self.assertIn('wait_for_file "$run_root/ACK" 172800', text)
        self.assertIn('plan-seed.txt', text)
        self.assertIn('plan_seed_args=(--seed "$plan_seed")', text)
        self.assertIn('run_root_created=1', text)
        self.assertIn('.shimmy-owned-run-root', text)
        self.assertIn('input-files.sha256', text)
        self.assertIn('cmp -- "$input_dir/plan.preview.json"', text)
        self.assertIn('safe-extract-tar-zst.py', text)
        runner_mode = 'chmod 0700 "$input_dir/agent-python-ultimate"'
        runner_plan = '"$input_dir/agent-python-ultimate" plan'
        self.assertIn(runner_mode, text)
        self.assertLess(text.index(runner_mode), text.index(runner_plan))
        self.assertNotIn('--max-output-size=', text)
        self.assertNotIn("/vol/bitbucket", text)

    def test_bundle_preparer_binds_source_plan_and_artifact(self) -> None:
        text = PREPARER.read_text(encoding="utf-8")
        for required in (
            "manual-only; refusing CI",
            "git diff --quiet",
            "main.sourceCommit=$source_commit",
            "plan-seed.txt",
            "plan.preview.json",
            "input-manifest.json",
            "input-files.sha256",
            "bundle.sha256",
            "agent-python-runtime-numpy-core.wasm",
            "input.tar.zst",
            "git ls-files --others",
            "git verify-commit",
            "git archive --format=tar",
        ):
            self.assertIn(required, text)
        self.assertIn('^[1-9][0-9]*$', text)
        self.assertIn('rm -r -- "$stage" "$source_tree"', text)

    def test_shell_entrypoints_parse(self) -> None:
        for script in (LAUNCHER, PREPARER, JOB_SCRIPT):
            with self.subTest(script=script):
                result = subprocess.run(
                    ["bash", "-n", str(script)],
                    cwd=ROOT,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_upload_requires_complete_bundle_and_private_modes(self) -> None:
        text = LAUNCHER.read_text(encoding="utf-8")
        body = text.split("upload_bundle() {", 1)[1].split("stage_job() {", 1)[0]
        for required in (
            "job.sh",
            "input.tar.zst",
            "input.sha256",
            "bundle.sha256",
            "0700",
            "0600",
            "shasum -a 256 -c bundle.sha256",
            "sha256sum -c bundle.sha256",
            "controller root already exists",
        ):
            self.assertIn(required, body)
        self.assertNotIn("sh -c", body)
        self.assertIn("verify_doc_bundle.py", body)
        self.assertIn('if ! mkdir -m 0700 -- "$root"', body)
        self.assertNotIn('[[ -e "$root" ]]', body)

    def test_pull_avoids_process_substitution_heredoc_on_macos_bash(self) -> None:
        text = LAUNCHER.read_text(encoding="utf-8")
        body = text.split("pull_result() {", 1)[1].split("ack_result() {", 1)[0]
        self.assertIn('transport_fields="$(python3 -', body)
        self.assertNotIn('read -r expected_bytes expected_hash < <(', body)

    def test_gateway_defaults_to_gpucluster2_and_reuses_control_connection(self) -> None:
        text = LAUNCHER.read_text(encoding="utf-8")
        self.assertIn('GATEWAY="${SHIMMY_DOC_GATEWAY:-gpucluster2}"', text)
        self.assertIn("ControlMaster=auto", text)
        self.assertIn("ControlPersist=15m", text)
        self.assertIn('ControlPath="$CONTROL_PATH"', text)
        self.assertIn("validate_gateway", text)

    def test_option_like_gateway_is_rejected_before_any_ssh(self) -> None:
        env = os.environ.copy()
        env["SHIMMY_DOC_GATEWAY"] = "-oProxyCommand=touch-pwned"
        result = subprocess.run(
            [str(LAUNCHER), "validate-run-id", "agent-python-20260727t001500z-a1b2c3d4"],
            cwd=ROOT,
            env=env,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("invalid SSH gateway", result.stderr)

    def test_preexisting_slurm_root_is_not_deleted_on_mkdir_failure(self) -> None:
        job_id = f"{os.getpid()}91"
        run_root = pathlib.Path(f"/tmp/shimmy-agent-python-{job_id}")
        shutil.rmtree(run_root, ignore_errors=True)
        run_root.mkdir(mode=0o700)
        marker = run_root / "victim"
        marker.write_text("keep\n", encoding="utf-8")
        try:
            env = os.environ.copy()
            env["SLURM_JOB_ID"] = job_id
            result = subprocess.run(
                [str(JOB_SCRIPT)],
                cwd=ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 2)
            self.assertTrue(marker.exists())
            self.assertEqual(marker.read_text(encoding="utf-8"), "keep\n")
        finally:
            shutil.rmtree(run_root, ignore_errors=True)

    def test_bundle_preparer_rejects_zero_seed_before_build(self) -> None:
        result = subprocess.run(
            [str(PREPARER), "/tmp/shimmy-zero-seed", "missing.json", "0"],
            cwd=ROOT,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("invalid plan seed", result.stderr)

    def test_pull_and_ack_are_separate_validation_phases(self) -> None:
        text = LAUNCHER.read_text(encoding="utf-8")
        pull_body = text.split("pull_result() {", 1)[1].split("ack_result() {", 1)[0]
        ack_body = text.split("ack_result() {", 1)[1].split("cleanup_controller() {", 1)[0]
        self.assertIn('--output "$extract_dir/output/run" --require-provenance', pull_body)
        self.assertIn('validation-receipt.json', pull_body)
        self.assertNotIn('mv -f --', pull_body)
        self.assertIn("ack_result() {", text)
        self.assertNotIn("sh -c", ack_body)
        self.assertIn('validation-receipt.json', ack_body)
        self.assertIn('result.tar.zst', ack_body)
        self.assertIn('[[ -f "$root/RESULT.READY" ]]', ack_body)
        self.assertIn('sha256sum "$root/result.tar.zst"', ack_body)
        self.assertIn('touch "$root/ACK"', ack_body)
        self.assertIn('ack) [[ $# -eq 3 ]] || usage; ack_result "$2" "$3"', text)

    def test_result_transport_is_bounded_before_local_write(self) -> None:
        text = LAUNCHER.read_text(encoding="utf-8")
        pull_body = text.split("pull_result() {", 1)[1].split("ack_result() {", 1)[0]
        self.assertIn("result.transport.json", pull_body)
        self.assertIn("receive_bounded_stdin", pull_body)
        self.assertIn("536870912", pull_body)
        self.assertNotIn("extractall", pull_body)

    def test_independent_bundle_verifier_rebuilds_signed_source(self) -> None:
        text = VERIFY_BUNDLE.read_text(encoding="utf-8")
        for phrase in ("verify-commit", "git", "archive", "go", "build", "plan preview differs"):
            self.assertIn(phrase, text)
        self.assertNotIn("extractall", text)


if __name__ == "__main__":
    unittest.main()
