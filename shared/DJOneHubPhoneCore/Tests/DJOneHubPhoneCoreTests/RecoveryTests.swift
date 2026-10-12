import XCTest
@testable import DJOneHubPhoneCore

extension PhoneTests {
  func testICERevisionRejectsRelabeledOldSDPAndCredentialReuse() {
    func sdp(_ id: Int, _ version: Int, _ u: String, _ p: String) -> String {
      "v=0\r\no=- \(id) \(version) IN IP4 127.0.0.1\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=ice-ufrag:\(u)\r\na=ice-pwd:\(p)\r\n"
    }
    let first = sdp(1,1,"first","firstPassword")
    let second = sdp(1,2,"second","secondPassword")
    XCTAssertTrue(ICERevision.isRestart(previous:first,next:second))
    XCTAssertFalse(ICERevision.isRestart(previous:second,next:first))
    XCTAssertFalse(ICERevision.isRestart(previous:first,next:sdp(2,2,"second","secondPassword")))
    XCTAssertFalse(ICERevision.isRestart(previous:first,next:sdp(1,2,"first","secondPassword")))
    XCTAssertFalse(ICERevision.isRestart(previous:first,next:sdp(1,2,"second","firstPassword")))
    XCTAssertFalse(ICERevision.isRestart(previous:first,next:second + "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n"))
  }
  @MainActor func testRestartPreservesCallAndRejectsOutOfOrderNegotiation() {
    var effects: [CallEffect] = []
    let call = PhoneCall(principal: principal()) { effects.append($0) }
    let first = invite(), b = first.binding!
    call.receive(first)
    call.reported(b, success: true)
    call.receive(Signal(type:"offer",binding:b,payload:["type":.string("offer"),"sdp":.string("v=0"),"recovery":.bool(true)]))
    call.mediaReady(b)
    call.requestAnswer(b)
    call.receive(Signal(type:"state",binding:b,payload:["revision":.uint(2),"state":.string("awaiting_audio")]))
    call.activated(b)
    call.receive(Signal(type:"state",binding:b,payload:["revision":.uint(3),"state":.string("connected")]))
    effects.removeAll()
    let restart = Signal(type:"restart_offer",binding:b,payload:["negotiation_id":.uint(1),"sdp":.string("v=0")])
    call.receive(restart)
    call.receive(restart)
    call.receive(Signal(type:"ice",binding:b,payload:["candidate":.string("old")]))
    XCTAssertEqual(call.binding,b)
    XCTAssertTrue(call.answered)
    XCTAssertEqual(effects.count,2)
    if case .acceptRestartOffer(let binding,_,let id) = effects[0] {
      XCTAssertEqual(binding,b); XCTAssertEqual(id,1)
    } else { XCTFail("restart replaced call or repeated answer") }
    var stale = restart
    stale.call_uuid = UUID().uuidString.lowercased()
    call.receive(stale)
    XCTAssertEqual(effects.count,2)
    var future = restart
    future.payload["negotiation_id"] = .uint(3)
    call.receive(future)
    XCTAssertNil(call.binding)
    XCTAssertEqual(call.endReason,"invalid_message")
  }
  @MainActor func testRestartCannotActivateUnansweredOrLegacyCall() {
    for enabled in [false,true] {
      let call = PhoneCall(principal:principal()) { _ in }
      let m = invite(), b = m.binding!
      call.receive(m)
      call.receive(Signal(type:"offer",binding:b,payload:["type":.string("offer"),"sdp":.string("v=0"),"recovery":.bool(enabled)]))
      call.receive(Signal(type:"restart_offer",binding:b,payload:["negotiation_id":.uint(1),"sdp":.string("v=0")]))
      XCTAssertNil(call.binding)
      XCTAssertFalse(call.answered)
    }
  }
}

extension PhoneTests {
  func testSignalRecoveryKeepsDeadlineAndRequiresCorrelatedCurrentState() throws {
    let b = invite().binding!, now = Date()
    var window = SignalRecoveryWindow()
    try window.begin(b, now: now)
    let deadline = window.deadline
    try window.begin(b, now: now.addingTimeInterval(9))
    XCTAssertEqual(window.deadline, deadline)
    let query = try window.synchronize(now: now)
    var state = Signal(type: "state", binding: b, payload: ["revision": .uint(5), "state": .string("connected")])
    XCTAssertFalse(try window.accept(state, revision: 5, now: now))
    state.request_id = query.request_id
    state.payload["revision"] = .uint(4)
    XCTAssertThrowsError(try window.accept(state, revision: 5, now: now))
    state.payload["revision"] = .uint(5)
    state.generation = b.generation + 1
    XCTAssertThrowsError(try window.accept(state, revision: 5, now: now))
    state.generation = b.generation
    XCTAssertThrowsError(try window.accept(state, revision: 5, now: now.addingTimeInterval(10)))
    XCTAssertEqual(window.deadline, deadline)
    XCTAssertTrue(try window.accept(state, revision: 5, now: now.addingTimeInterval(9)))
    XCTAssertNil(window.binding)
    XCTAssertNil(window.deadline)
  }
  func testRecoveryDoesNotFollowNewCallAndAcceptsAuthoritativeEnd() throws {
    let b = invite().binding!, now = Date()
    var window = SignalRecoveryWindow()
    try window.begin(b, now: now)
    XCTAssertThrowsError(try window.begin(invite().binding!, now: now))
    let query = try window.synchronize(now: now)
    let ended = Signal(type: "state", binding: b, requestID: query.request_id!,
      payload: ["revision": .uint(6), "state": .string("ended")])
    XCTAssertTrue(try window.accept(ended, revision: 5, now: now))
  }
}

extension PhoneTests {
  @MainActor func testAudioInterruptionResumesSameCallAndHasFixedDeadline() {
    for scenario in ["resume", "timeout", "legacy", "late_activation", "hangup"] {
      var effects: [CallEffect] = []
      let call = PhoneCall(principal: principal()) { effects.append($0) }
      let first = invite(), b = first.binding!, now = Date()
      call.receive(first)
      call.reported(b, success: true)
      call.receive(Signal(type: "offer", binding: b, payload: ["type": .string("offer"), "sdp": .string("v=0"), "recovery": .bool(true), "audio_recovery": .bool(scenario != "legacy")]))
      call.mediaReady(b)
      call.requestAnswer(b)
      call.receive(Signal(type: "state", binding: b, payload: ["revision": .uint(2), "state": .string("awaiting_audio")]))
      call.activated(b)
      call.receive(Signal(type: "state", binding: b, payload: ["revision": .uint(3), "state": .string("connected")]))
      effects.removeAll()
      call.deactivated(b, now: now)
      if scenario == "legacy" { XCTAssertNil(call.binding); continue }
      XCTAssertEqual(call.binding, b)
      XCTAssertTrue(call.answered)
      XCTAssertEqual(effects.count, 3)
      if case .audio(let binding, let active) = effects[0] { XCTAssertEqual(binding, b); XCTAssertFalse(active) }
      else { XCTFail("audio did not stop first") }
      if case .send(let message) = effects[1] { XCTAssertEqual(message.payload["sequence"]?.uint, 2); XCTAssertEqual(message.payload["active"]?.bool, false) }
      else { XCTFail("Hub was not notified") }
      call.deactivated(b, now: now.addingTimeInterval(9))
      XCTAssertEqual(effects.count, 3, "Repeated interruption extended or replayed pause")
      // State reconciliation during pause must preserve the CallKit gate.
      call.receive(Signal(type: "state", binding: b, payload: ["revision": .uint(4), "state": .string("connected")]))
      XCTAssertEqual(call.binding,b)
      switch scenario {
      case "resume":
        call.activated(b, now: now.addingTimeInterval(2))
        call.tick(now: now.addingTimeInterval(11))
        XCTAssertEqual(call.binding,b)
        let answerEffects = effects.filter { if case .answerCompleted = $0 { return true }; return false }
        XCTAssertTrue(answerEffects.isEmpty, "Recovery repeated the CallKit answer")
      case "late_activation":
        call.activated(b, now: now.addingTimeInterval(10))
        XCTAssertNil(call.binding)
      case "hangup":
        call.receive(Signal(type: "state", binding: b, payload: ["revision": .uint(5), "state": .string("ended")]))
        call.activated(b, now: now.addingTimeInterval(2))
        XCTAssertNil(call.binding)
        let next = invite()
        call.receive(next)
        call.tick(now: now.addingTimeInterval(11))
        XCTAssertEqual(call.binding, next.binding, "Old interruption deadline reached the next call")
      default:
        call.tick(now: now.addingTimeInterval(10))
        XCTAssertNil(call.binding)
      }
    }
  }
}

extension PhoneTests {
  @MainActor func testCallControlsAreNegotiatedBoundAndNeverReplayDTMF() {
    for enabled in [false,true] {
      var sent: [Signal] = []
      var completed: [(String,Bool)] = []
      let call = PhoneCall(principal: principal()) {
        if case .send(let m) = $0 { sent.append(m) }
        if case .controlCompleted(_,let id,let ok) = $0 { completed.append((id,ok)) }
      }
      let first = invite(), b = first.binding!, now = Date()
      call.receive(first, now: now)
      XCTAssertNil(call.requestDTMF(b,digit:"1",now:now))
      call.reported(b,success:true)
      call.receive(Signal(type:"offer",binding:b,payload:["type":.string("offer"),"sdp":.string("v=0"),"call_controls":.bool(enabled)]),now:now)
      call.mediaReady(b); call.requestAnswer(b,now:now)
      call.receive(Signal(type:"state",binding:b,payload:["revision":.uint(2),"state":.string("awaiting_audio")]),now:now)
      call.activated(b,now:now)
      // Local mute during initial activation must be synchronized once connected.
      call.setMuted(b,muted:true,now:now)
      call.receive(Signal(type:"state",binding:b,payload:["revision":.uint(3),"state":.string("connected")]),now:now)
      if !enabled {
        XCTAssertNil(call.requestDTMF(b,digit:"1",now:now))
        XCTAssertFalse(sent.contains { $0.type == "mute" })
        continue
      }
      let mute = sent.last { $0.type == "mute" }!
      XCTAssertEqual(mute.payload["sequence"],.uint(1))
      XCTAssertEqual(mute.payload["muted"],.bool(true))
      call.setMuted(b,muted:false,now:now)
      let unmute = sent.last!
      XCTAssertEqual(unmute.payload["sequence"],.uint(2))
      call.receive(Signal(type:"result",binding:b,requestID:mute.request_id!,payload:["accepted":.bool(true)]),now:now)
      XCTAssertTrue(completed.isEmpty) // stale mute ack cannot confirm newer unmute
      call.receive(Signal(type:"result",binding:b,requestID:unmute.request_id!,payload:["accepted":.bool(true)]),now:now)
      XCTAssertEqual(completed.count,1)
      let id = call.requestDTMF(b,digit:"#",now:now)!
      XCTAssertEqual(sent.last?.payload["digit"],.string("#"))
      XCTAssertNil(call.requestDTMF(b,digit:"5",now:now.addingTimeInterval(1)))
      let count = sent.count
      call.tick(now:now.addingTimeInterval(5))
      XCTAssertEqual(sent.count,count) // no retry on uncertain outcome
      XCTAssertEqual(completed.last?.0,id)
      XCTAssertEqual(completed.last?.1,false)
      XCTAssertEqual(call.binding,b) // a failed key must not end the call
      for digit in ["", "12", "A", "1;ATH", "١"] {
        XCTAssertNil(call.requestDTMF(b,digit:digit,now:now.addingTimeInterval(6)))
      }
      let next = call.requestDTMF(b,digit:"*",now:now.addingTimeInterval(6))!
      call.receive(Signal(type:"result",binding:b,requestID:next,payload:["accepted":.bool(true)]),now:now.addingTimeInterval(6))
      XCTAssertEqual(completed.last?.1,true)
      XCTAssertNil(call.requestDTMF(b,digit:"*",now:now.addingTimeInterval(6.01)))
      let pending = call.requestDTMF(b,digit:"0",now:now.addingTimeInterval(7))!
      call.requestEnd(b,now:now.addingTimeInterval(7))
      XCTAssertEqual(completed.last?.0,pending)
      XCTAssertEqual(completed.last?.1,false)
      XCTAssertNil(call.requestDTMF(b,digit:"1",now:now.addingTimeInterval(8)))
    }
  }
}
