/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.AccessMode
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.HostBackend
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PVCBackend
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class VolumeModelsTest {

    @Test
    fun `HostBackend should require absolute path`() {
        val backend = HostBackend.of("/data/shared")
        assertEquals("/data/shared", backend.path)
    }

    @Test
    fun `HostBackend should reject relative path`() {
        assertThrows(IllegalArgumentException::class.java) {
            HostBackend.of("relative/path")
        }
    }

    @Test
    fun `PVCBackend should accept valid claim name`() {
        val backend = PVCBackend.of("my-pvc")
        assertEquals("my-pvc", backend.claimName)
    }

    @Test
    fun `PVCBackend should reject blank claim name`() {
        assertThrows(IllegalArgumentException::class.java) {
            PVCBackend.of("   ")
        }
    }

    @Test
    fun `Volume with host backend should be created correctly`() {
        val volume = Volume.builder()
            .name("data")
            .host(HostBackend.of("/data/shared"))
            .mountPath("/mnt/data")
            .accessMode(AccessMode.RW)
            .build()

        assertEquals("data", volume.name)
        assertNotNull(volume.host)
        assertEquals("/data/shared", volume.host?.path)
        assertNull(volume.pvc)
        assertEquals("/mnt/data", volume.mountPath)
        assertEquals(AccessMode.RW, volume.accessMode)
        assertNull(volume.subPath)
    }

    @Test
    fun `Volume with PVC backend should be created correctly`() {
        val volume = Volume.builder()
            .name("models")
            .pvc(PVCBackend.of("shared-models"))
            .mountPath("/mnt/models")
            .accessMode(AccessMode.RO)
            .subPath("v1")
            .build()

        assertEquals("models", volume.name)
        assertNull(volume.host)
        assertNotNull(volume.pvc)
        assertEquals("shared-models", volume.pvc?.claimName)
        assertEquals("/mnt/models", volume.mountPath)
        assertEquals(AccessMode.RO, volume.accessMode)
        assertEquals("v1", volume.subPath)
    }

    @Test
    fun `Volume should reject blank name`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .name("   ")
                .host(HostBackend.of("/data"))
                .mountPath("/mnt")
                .accessMode(AccessMode.RW)
                .build()
        }
    }

    @Test
    fun `Volume should require absolute mount path`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .name("test")
                .host(HostBackend.of("/data"))
                .mountPath("relative/path")
                .accessMode(AccessMode.RW)
                .build()
        }
    }

    @Test
    fun `Volume should reject no backend specified`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .name("test")
                .mountPath("/mnt")
                .accessMode(AccessMode.RW)
                .build()
        }
    }

    @Test
    fun `Volume should reject multiple backends specified`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .name("test")
                .host(HostBackend.of("/data"))
                .pvc(PVCBackend.of("my-pvc"))
                .mountPath("/mnt")
                .accessMode(AccessMode.RW)
                .build()
        }
    }

    @Test
    fun `Volume should require name`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .host(HostBackend.of("/data"))
                .mountPath("/mnt")
                .accessMode(AccessMode.RW)
                .build()
        }
    }

    @Test
    fun `Volume should require mount path`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .name("test")
                .host(HostBackend.of("/data"))
                .accessMode(AccessMode.RW)
                .build()
        }
    }

    @Test
    fun `Volume should require access mode`() {
        assertThrows(IllegalArgumentException::class.java) {
            Volume.builder()
                .name("test")
                .host(HostBackend.of("/data"))
                .mountPath("/mnt")
                .build()
        }
    }
}
