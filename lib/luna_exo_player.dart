import 'dart:async';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:media_kit/media_kit.dart';
import 'package:media_kit/src/models/player_log.dart';
import 'package:media_kit/src/models/player_stream.dart';
import 'package:media_kit/src/player/platform_player.dart';
import 'package:video_player/video_player.dart';

import 'diary_service.dart';

class LunaPlayerStreams implements PlayerStream {
  final StreamController<Duration> positionController =
      StreamController<Duration>.broadcast();
  final StreamController<Duration> durationController =
      StreamController<Duration>.broadcast();
  final StreamController<Duration> bufferController =
      StreamController<Duration>.broadcast();
  final StreamController<bool> playingController =
      StreamController<bool>.broadcast();
  final StreamController<bool> bufferingController =
      StreamController<bool>.broadcast();
  final StreamController<bool> completedController =
      StreamController<bool>.broadcast();
  final StreamController<String> errorController =
      StreamController<String>.broadcast();
  final StreamController<double> volumeController =
      StreamController<double>.broadcast();
  final StreamController<double> rateController =
      StreamController<double>.broadcast();
  final StreamController<VideoParams> videoParamsController =
      StreamController<VideoParams>.broadcast();
  final StreamController<PlayerLog> logController =
      StreamController<PlayerLog>.broadcast();
  final StreamController<double> pitchController =
      StreamController<double>.broadcast();

  @override
  Stream<Duration> get position => positionController.stream;

  @override
  Stream<Duration> get duration => durationController.stream;

  @override
  Stream<Duration> get buffer => bufferController.stream;

  @override
  Stream<bool> get playing => playingController.stream;

  @override
  Stream<bool> get buffering => bufferingController.stream;

  @override
  Stream<bool> get completed => completedController.stream;

  @override
  Stream<String> get error => errorController.stream;

  @override
  Stream<double> get volume => volumeController.stream;

  @override
  Stream<double> get rate => rateController.stream;

  @override
  Stream<VideoParams> get videoParams => videoParamsController.stream;

  @override
  Stream<PlayerLog> get log => logController.stream;

  @override
  Stream<double> get pitch => pitchController.stream;

  void dispose() {
    positionController.close();
    durationController.close();
    bufferController.close();
    playingController.close();
    bufferingController.close();
    completedController.close();
    errorController.close();
    volumeController.close();
    rateController.close();
    videoParamsController.close();
    logController.close();
    pitchController.close();
  }

  @override
  dynamic noSuchMethod(Invocation invocation) {
    if (invocation.isGetter) {
      return const Stream<dynamic>.empty();
    }
    return null;
  }
}

class LunaExoPlayer implements Player {
  LunaExoPlayer() {
    state = const PlayerState().copyWith(volume: 100.0, rate: 1.0);
  }

  VideoPlayerController? _controller;
  VoidCallback? _valueListener;
  Timer? _positionPollTimer;

  int _openGeneration = 0;
  bool _disposed = false;
  String? _lastReportedError;
  int _lastWidth = 0;
  int _lastHeight = 0;

  final ValueNotifier<int> revision = ValueNotifier<int>(0);

  @override
  PlayerState state = const PlayerState();

  @override
  final LunaPlayerStreams stream = LunaPlayerStreams();

  @override
  PlatformPlayer? get platform => null;

  @override
  Future<int> get handle => Future<int>.value(0);

  VideoPlayerController? get controller => _controller;

  bool _isLoopbackHost(String host) {
    final lower = host.toLowerCase();
    return lower == '127.0.0.1' || lower == 'localhost' || lower == '::1';
  }

  Future<bool> _sniffRemoteHls(Uri uri, Map<String, String> headers) async {
    HttpClient? client;
    try {
      client = HttpClient()..connectionTimeout = const Duration(seconds: 2);
      final req = await client.getUrl(uri).timeout(const Duration(seconds: 2));
      headers.forEach((k, v) => req.headers.set(k, v));
      req.headers.set('Range', 'bytes=0-512');
      final resp = await req.close().timeout(const Duration(seconds: 2));
      final cType = resp.headers.contentType?.toString().toLowerCase() ?? '';
      if (cType.contains('mpegurl')) {
        return true;
      }
      if (cType.contains('video/mp4')) {
        return false;
      }
      final chunk = await resp
          .take(1)
          .fold<List<int>>(<int>[], (acc, data) => acc..addAll(data))
          .timeout(const Duration(seconds: 2));
      final preview = chunk.isNotEmpty
          ? String.fromCharCodes(chunk.take(64)).trimLeft()
          : '';
      return preview.startsWith('#EXTM3U') || preview.startsWith('\ufeff#EXTM3U');
    } catch (e) {
      DiaryService.add('[Sniff] 远端格式探测跳过: $e');
      return false;
    } finally {
      client?.close(force: true);
    }
  }

  Future<void> _releaseController(VideoPlayerController? target) async {
    if (target == null) return;
    if (identical(target, _controller)) {
      if (_valueListener != null) {
        try {
          target.removeListener(_valueListener!);
        } catch (_) {}
        _valueListener = null;
      }
      _controller = null;
    }
    try {
      await target.pause();
    } catch (_) {}
    try {
      await target.dispose();
    } catch (_) {}
  }

  @override
  Future<void> open(Playable playable, {bool play = true}) async {
    if (_disposed) return;
    if (playable is! Media) return;

    final media = playable;
    final url = media.uri;
    if (url.isEmpty) return;

    final myGen = ++_openGeneration;
    _lastReportedError = null;
    _lastWidth = 0;
    _lastHeight = 0;
    _positionPollTimer?.cancel();

    final old = _controller;
    if (old != null) {
      await _releaseController(old);
      _bumpRevision();
    }

    if (_disposed || myGen != _openGeneration) return;

    state = state.copyWith(
      completed: false,
      playing: false,
      buffering: true,
      position: Duration.zero,
      buffer: Duration.zero,
    );
    _safeAdd(stream.completedController, false);
    _safeAdd(stream.playingController, false);
    _safeAdd(stream.bufferingController, true);

    final Map<String, String> headers = <String, String>{};
    try {
      if (media.httpHeaders != null) {
        headers.addAll(Map<String, String>.from(media.httpHeaders!));
      }
    } catch (_) {}

    if (!headers.keys.any((k) => k.toLowerCase() == 'user-agent')) {
      headers['User-Agent'] =
          'Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36';
    }

    final uri = Uri.tryParse(url);
    final isLocalFile =
        url.startsWith('/') || (uri != null && uri.scheme == 'file');
    if (!isLocalFile && (uri == null || (uri.scheme != 'http' && uri.scheme != 'https'))) {
      DiaryService.add('[ExoPlayer] 错误: 无效的播放地址 $url');
      throw Exception('无效的播放地址: $url');
    }

    bool isHls = false;
    if (!isLocalFile && uri != null) {
      final pathLower = uri.path.toLowerCase();
      final acceptHeader = headers.entries
          .where((e) => e.key.toLowerCase() == 'accept')
          .map((e) => e.value.toLowerCase())
          .join(',');
      if (pathLower.endsWith('.m3u8') || acceptHeader.contains('mpegurl')) {
        isHls = true;
      } else if (!pathLower.endsWith('.mp4') &&
          !pathLower.endsWith('.m4s') &&
          !_isLoopbackHost(uri.host)) {
        isHls = await _sniffRemoteHls(uri, headers);
        if (_disposed || myGen != _openGeneration) return;
      }
    }

    VideoFormat? formatHint = isHls ? VideoFormat.hls : null;
    DiaryService.add(
      '[ExoPlayer] open(gen=$myGen): url=$url, formatHint=$formatHint, local=$isLocalFile',
    );

    VideoPlayerController c;
    if (isLocalFile) {
      final filePath =
          uri != null && uri.scheme == 'file' ? uri.toFilePath() : url;
      c = VideoPlayerController.file(
        File(filePath),
        videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
      );
    } else {
      c = VideoPlayerController.networkUrl(
        uri!,
        formatHint: formatHint,
        httpHeaders: headers,
        videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
      );
    }

    try {
      await c.initialize();
      DiaryService.add(
        '[ExoPlayer] initialize 成功: duration=${c.value.duration}, size=${c.value.size}',
      );
    } catch (e) {
      DiaryService.add('[ExoPlayer] 首次 initialize 失败 (formatHint=$formatHint): $e');
      if (!isLocalFile && uri != null && !_disposed && myGen == _openGeneration) {
        final alternateFormat =
            formatHint == VideoFormat.hls ? null : VideoFormat.hls;
        DiaryService.add('[ExoPlayer] 切换 formatHint=$alternateFormat 重试...');
        try {
          await c.dispose();
        } catch (_) {}

        final retryHeaders = Map<String, String>.from(headers);
        if (alternateFormat == VideoFormat.hls) {
          retryHeaders['Accept'] =
              'application/vnd.apple.mpegurl,application/x-mpegURL,*/*';
        }
        c = VideoPlayerController.networkUrl(
          uri,
          formatHint: alternateFormat,
          httpHeaders: retryHeaders,
          videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
        );
        try {
          await c.initialize();
          formatHint = alternateFormat;
          DiaryService.add(
            '[ExoPlayer] 交替格式重试成功: duration=${c.value.duration}, size=${c.value.size}',
          );
        } catch (retryErr) {
          DiaryService.add('[ExoPlayer] 重试失败: $retryErr');
          try {
            await c.dispose();
          } catch (_) {}
          rethrow;
        }
      } else {
        try {
          await c.dispose();
        } catch (_) {}
        rethrow;
      }
    }

    if (_disposed || myGen != _openGeneration) {
      try {
        await c.dispose();
      } catch (_) {}
      return;
    }

    _controller = c;
    final dur = c.value.duration;
    final w = c.value.size.width.round();
    final h = c.value.size.height.round();
    _lastWidth = w;
    _lastHeight = h;
    final videoParams = w > 0 && h > 0
        ? VideoParams(w: w, h: h, dw: w, dh: h)
        : const VideoParams();

    state = state.copyWith(
      duration: dur,
      position: Duration.zero,
      playing: play,
      buffering: c.value.isBuffering,
      width: w > 0 ? w : null,
      height: h > 0 ? h : null,
      videoParams: videoParams,
    );

    _safeAdd(stream.durationController, dur);
    _safeAdd(stream.positionController, Duration.zero);
    _safeAdd(stream.playingController, play);
    _safeAdd(stream.bufferingController, c.value.isBuffering);
    if (w > 0 && h > 0) {
      _safeAdd(stream.videoParamsController, videoParams);
    }

    _valueListener = () => _onControllerValue(c, myGen);
    c.addListener(_valueListener!);
    _startPositionPolling();

    try {
      await c.setVolume((state.volume / 100.0).clamp(0.0, 1.0));
    } catch (_) {}
    if (state.rate != 1.0) {
      try {
        await c.setPlaybackSpeed(state.rate.clamp(0.25, 4.0));
      } catch (_) {}
    }

    if (media.start != null && media.start! > Duration.zero) {
      try {
        await c.seekTo(media.start!);
        if (!_disposed && myGen == _openGeneration) {
          state = state.copyWith(position: media.start!);
          _safeAdd(stream.positionController, media.start!);
        }
      } catch (_) {}
    }

    if (play && !_disposed && myGen == _openGeneration) {
      try {
        await c.play();
      } catch (_) {}
    }
    _bumpRevision();
  }

  void _startPositionPolling() {
    _positionPollTimer?.cancel();
    _positionPollTimer = Timer.periodic(const Duration(milliseconds: 250), (_) {
      final c = _controller;
      if (_disposed || c == null || !c.value.isInitialized) return;
      final pos = c.value.position;
      if (pos != state.position) {
        state = state.copyWith(position: pos);
        _safeAdd(stream.positionController, pos);
      }
    });
  }

  void _onControllerValue(VideoPlayerController c, int generation) {
    if (_disposed || !identical(_controller, c) || generation != _openGeneration) {
      return;
    }
    final v = c.value;

    bool stateChanged = false;

    if (v.hasError) {
      final desc = (v.errorDescription ?? 'ExoPlayer playback error').trim();
      if (desc.isNotEmpty && desc != _lastReportedError) {
        _lastReportedError = desc;
        DiaryService.add('[ExoPlayer] 运行期错误: $desc');
        _safeAdd(stream.errorController, desc);
      }
    }

    final w = v.size.width.round();
    final h = v.size.height.round();
    if (w > 0 && h > 0 && (w != _lastWidth || h != _lastHeight)) {
      _lastWidth = w;
      _lastHeight = h;
      final params = VideoParams(w: w, h: h, dw: w, dh: h);
      state = state.copyWith(width: w, height: h, videoParams: params);
      _safeAdd(stream.videoParamsController, params);
      stateChanged = true;
    }

    if (v.isPlaying != state.playing) {
      state = state.copyWith(playing: v.isPlaying);
      _safeAdd(stream.playingController, v.isPlaying);
      stateChanged = true;
    }
    if (v.isBuffering != state.buffering) {
      state = state.copyWith(buffering: v.isBuffering);
      _safeAdd(stream.bufferingController, v.isBuffering);
      stateChanged = true;
    }
    final dur = v.duration;
    if (dur > Duration.zero && dur != state.duration) {
      state = state.copyWith(duration: dur);
      _safeAdd(stream.durationController, dur);
      stateChanged = true;
    }
    final pos = v.position;
    if (pos != state.position) {
      state = state.copyWith(position: pos);
      _safeAdd(stream.positionController, pos);
    }
    if (v.buffered.isNotEmpty) {
      final lastRange = v.buffered.last;
      if (lastRange.end != state.buffer) {
        state = state.copyWith(buffer: lastRange.end);
        _safeAdd(stream.bufferController, state.buffer);
      }
    }
    if (v.isCompleted != state.completed) {
      state = state.copyWith(completed: v.isCompleted);
      _safeAdd(stream.completedController, v.isCompleted);
      stateChanged = true;
    }

    if (stateChanged) {
      _bumpRevision();
    }
  }

  @override
  Future<void> play() async {
    final c = _controller;
    if (_disposed || c == null) return;
    try {
      await c.play();
    } catch (_) {}
  }

  @override
  Future<void> pause() async {
    final c = _controller;
    if (_disposed || c == null) return;
    try {
      await c.pause();
    } catch (_) {}
  }

  @override
  Future<void> playOrPause() async {
    final c = _controller;
    if (_disposed || c == null) return;
    if (c.value.isPlaying) {
      await pause();
    } else {
      await play();
    }
  }

  @override
  Future<void> seek(Duration target) async {
    final c = _controller;
    if (_disposed || c == null) return;
    try {
      await c.seekTo(target);
      if (_disposed || !identical(_controller, c)) return;
      state = state.copyWith(position: target);
      _safeAdd(stream.positionController, target);
      if (state.completed) {
        state = state.copyWith(completed: false);
        _safeAdd(stream.completedController, false);
      }
    } catch (_) {}
  }

  @override
  Future<void> setRate(double rate) async {
    final r = rate.clamp(0.25, 4.0);
    state = state.copyWith(rate: r);
    _safeAdd(stream.rateController, r);
    final c = _controller;
    if (_disposed || c == null) return;
    try {
      await c.setPlaybackSpeed(r);
    } catch (_) {}
  }

  @override
  Future<void> setVolume(double volume) async {
    final v = volume.clamp(0.0, 100.0);
    state = state.copyWith(volume: v);
    _safeAdd(stream.volumeController, v);
    final c = _controller;
    if (_disposed || c == null) return;
    try {
      await c.setVolume(v / 100.0);
    } catch (_) {}
  }

  @override
  Future<void> stop() async {
    _openGeneration++;
    _positionPollTimer?.cancel();
    final c = _controller;
    if (c != null) {
      await _releaseController(c);
    }
    state = state.copyWith(
      playing: false,
      buffering: false,
      completed: false,
      position: Duration.zero,
      buffer: Duration.zero,
    );
    _safeAdd(stream.playingController, false);
    _safeAdd(stream.bufferingController, false);
    _safeAdd(stream.completedController, false);
    _safeAdd(stream.positionController, Duration.zero);
    _safeAdd(stream.bufferController, Duration.zero);
    _bumpRevision();
  }

  @override
  Future<void> dispose() async {
    if (_disposed) return;
    _disposed = true;
    _openGeneration++;
    _positionPollTimer?.cancel();
    await _releaseController(_controller);
    stream.dispose();
    revision.dispose();
  }

  void _bumpRevision() {
    if (!_disposed) {
      revision.value++;
    }
  }

  void _safeAdd<T>(StreamController<T> ctl, T value) {
    if (!_disposed && !ctl.isClosed) ctl.add(value);
  }

  @override
  dynamic noSuchMethod(Invocation invocation) {
    if (invocation.isGetter) return null;
    if (invocation.isMethod) return Future<void>.value();
    return null;
  }
}

class LunaExoVideoView extends StatelessWidget {
  const LunaExoVideoView({
    super.key,
    required this.player,
    this.fit = BoxFit.contain,
    this.controls,
  });

  final LunaExoPlayer player;
  final BoxFit fit;
  final Widget Function(BuildContext)? controls;

  @override
  Widget build(BuildContext context) {
    return AnimatedBuilder(
      animation: player.revision,
      builder: (context, _) {
        final c = player.controller;
        final hasVideo = c != null && c.value.isInitialized;
        final videoSize = hasVideo ? c.value.size : Size.zero;
        final rawRatio = hasVideo ? c.value.aspectRatio : 16 / 9;
        final ratio = (rawRatio > 0 && !rawRatio.isNaN && !rawRatio.isInfinite)
            ? rawRatio
            : 16 / 9;
        final width = videoSize.width > 0 ? videoSize.width : ratio * 1000;
        final height = videoSize.height > 0 ? videoSize.height : 1000.0;

        return Stack(
          fit: StackFit.expand,
          children: [
            const ColoredBox(color: Colors.black),
            if (hasVideo)
              ClipRect(
                child: FittedBox(
                  fit: fit,
                  child: SizedBox(
                    width: width,
                    height: height,
                    child: VideoPlayer(c),
                  ),
                ),
              ),
            if (controls != null) controls!(context),
          ],
        );
      },
    );
  }
}
