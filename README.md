# 🚀 StreamSrv — Real-Time WebRTC Streaming Platform

🎥 **Demo**  
https://youtu.be/y7a8Yw6csIc (with audio)
https://youtu.be/po8Xv_Q1oNA (no audio)

---

## ⚡ Overview

StreamSrv is a low-latency real-time streaming platform built with WebRTC that enables users to stream screen, camera, and audio directly to multiple viewers using peer-to-peer connections.

Unlike traditional streaming systems, **media is not processed or relayed through the server**. The backend handles only signaling and coordination, while video/audio flows directly between clients, resulting in minimal latency and efficient resource usage.

---

## 🔥 Key Highlights

- ⚡ Peer-to-peer streaming (no server media relay)
- 👥 Multi-viewer architecture (1 → N)
- 🎥 Screen sharing + camera overlay (PiP)
- 🎤 Independent mic / cam / screen control
- 💬 Real-time chat via WebSocket
- 🧠 Adaptive quality (ABR using WebRTC stats)
- 📊 Latency monitoring (RTT indicator)
- 🔐 JWT authentication system
- 🗄 PostgreSQL persistence (users, streams, chat)
- 🔄 Auto-reconnect with backoff

---

## 🏗 Architecture

The system follows a peer-to-peer real-time architecture:

- **Backend (Go)**
  - WebSocket signaling server
  - Handles room management (join/leave)
  - Exchanges SDP (offer/answer) and ICE candidates
  - Provides REST API and JWT authentication
  - Does NOT process or relay media

- **Frontend (JavaScript)**
  - Uses WebRTC APIs for media streaming
  - Creates RTCPeerConnection per viewer
  - Handles screen capture, camera, and audio streams
  - Uses Web Audio API for audio control

- **Media Flow**
  - Streamer → multiple viewers (1 → N)
  - Direct peer-to-peer connections
  - No media passes through the server

- **Scaling Model**
  - Each viewer establishes an independent connection
  - Server load remains minimal (signaling only)

---

## 🌍 Deployment (Previous Version Demo)
🎥 **Demo** https://youtu.be/3hzuK660YO8
This demo videos show a previously deployed version of the platform running on Fly.io.

That version included:
- Public internet streaming (not LAN)
- Real-time chat
- Camera + screen streaming
- Multi-user rooms

Due to loss of source files, this repository contains a rebuilt and improved version of the system with cleaner architecture and extended functionality.
