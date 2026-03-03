#!/usr/bin/env python3
"""
Test script to create a Kata Containers sandbox using OpenSandbox SDK
"""
import asyncio
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig

async def main():
    print("=" * 60)
    print("  OpenSandbox SDK - Kata Containers Test")
    print("=" * 60)
    
    # Initialize connection config
    print("\n[1/4] Connecting to OpenSandbox Server (http://localhost:8080)...")
    config = ConnectionConfig(
        domain="localhost:8080",
        protocol="http",
        request_timeout=timedelta(seconds=300),
        use_server_proxy=True,  # Use server as proxy since Pod IP is not directly accessible
    )
    
    # Create sandbox with Kata runtime (configured in server)
    print("\n[2/4] Creating sandbox with Kata Containers (QEMU) runtime...")
    print("      Image: alpine:3.19")
    print("      RuntimeClass: kata-qemu (server-side config)")
    
    try:
        sandbox = await Sandbox.create(
            "opensandbox/task-executor:v1.0.0",
            timeout=timedelta(minutes=30),
            ready_timeout=timedelta(seconds=120),
            env={"TEST_ENV": "kata-sandbox"},
            metadata={"name": "kata-sdk-test", "runtime": "kata-qemu"},
            resource={"cpu": "200m", "memory": "256Mi"},
            entrypoint=["sh", "-c", "echo 'Kata sandbox via SDK!' && uname -a && sleep 600"],
            connection_config=config,
        )
        
        print(f"\n[3/4] Sandbox created successfully!")
        print(f"      Sandbox ID: {sandbox.id}")
        
        # Get sandbox info
        info = await sandbox.get_info()
        print(f"\n[4/4] Sandbox Status:")
        print(f"      State: {info.status.state}")
        print(f"      Metadata: {info.metadata}")
        print(f"      Expires: {info.expires_at}")
        
        # Run a command to verify Kata runtime
        print("\n" + "=" * 60)
        print("  Running verification commands...")
        print("=" * 60)
        
        # Check kernel version (should be different from host if Kata is working)
        result = await sandbox.commands.run("uname -r")
        kernel = result.logs.stdout[0].text.strip() if result.logs.stdout else "N/A"
        print(f"\n  Kernel version: {kernel}")
        
        # Check if running in hypervisor
        result = await sandbox.commands.run("grep -o hypervisor /proc/cpuinfo | head -1 || echo 'not found'")
        hypervisor = result.logs.stdout[0].text.strip() if result.logs.stdout else "N/A"
        print(f"  Hypervisor flag: {hypervisor}")
        
        print("\n" + "=" * 60)
        print("  SUCCESS! Kata Containers sandbox is running!")
        print("=" * 60)
        print(f"\n  Sandbox ID: {sandbox.id}")
        print(f"\n  Clean up command:")
        print(f"    kubectl delete batchsandbox {sandbox.id} -n opensandbox")
        
        # Don't kill sandbox - leave it running for verification
        await sandbox.close()
        
        return sandbox.id
        
    except Exception as e:
        print(f"\n  ERROR: {e}")
        import traceback
        traceback.print_exc()
        return None

if __name__ == "__main__":
    asyncio.run(main())
