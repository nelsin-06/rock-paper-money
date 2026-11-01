import React, { useState, useEffect, useRef } from 'react';
import { Hands } from '@mediapipe/hands';
import { Camera } from '@mediapipe/camera_utils';

export default function SmartGameCamera({ onPlaySelected }) {
  // --- ESTADOS Y REFS DE CÁMARA Y DETECCIÓN ---
  const [isCamOn, setIsCamOn] = useState(false);
  const [detectedChoice, setDetectedChoice] = useState(null); // 'PIEDRA', 'PAPEL', 'TIJERA'
  const [manualChoice, setManualChoice] = useState(null);
  
  const videoRef = useRef(null);
  const cameraRef = useRef(null);

  // --- ESTADOS Y REFS DEL CRONÓMETRO ---
  const [timeLeft, setTimeLeft] = useState(20);
  const [isTurnActive, setIsTurnActive] = useState(false);

  // Función para clasificar el gesto de la mano según la posición de las articulaciones
  const detectGesture = (landmarks) => {
    // Dedos extendidos comparando la punta (tip) con la articulación media (pip)
    const indexExtended = landmarks[8].y < landmarks[6].y;
    const middleExtended = landmarks[12].y < landmarks[10].y;
    const ringExtended = landmarks[16].y < landmarks[14].y;
    const pinkyExtended = landmarks[20].y < landmarks[18].y;

    if (indexExtended && middleExtended && !ringExtended && !pinkyExtended) {
      return 'TIJERA';
    } else if (indexExtended && middleExtended && ringExtended && pinkyExtended) {
      return 'PAPEL';
    } else if (!indexExtended && !middleExtended && !ringExtended && !pinkyExtended) {
      return 'PIEDRA';
    }
    return null;
  };

  // Configuración de MediaPipe Hands al encender la cámara
  useEffect(() => {
    if (!isCamOn || !videoRef.current) return;

    const hands = new Hands({
      locateFile: (file) => `https://cdn.jsdelivr.net/npm/@mediapipe/hands/${file}`,
    });

    hands.setOptions({
      maxNumHands: 1,
      modelComplexity: 1,
      minDetectionConfidence: 0.6,
      minTrackingConfidence: 0.6,
    });

    hands.onResults((results) => {
      if (results.multiHandLandmarks && results.multiHandLandmarks.length > 0) {
        const gesture = detectGesture(results.multiHandLandmarks[0]);
        if (gesture) {
          setDetectedChoice(gesture);
        }
      }
    });

    const camera = new Camera(videoRef.current, {
      onFrame: async () => {
        if (videoRef.current) {
          await hands.send({ image: videoRef.current });
        }
      },
      width: 320,
      height: 240,
    });

    camera.start();
    cameraRef.current = camera;

    return () => {
      camera.stop();
      hands.close();
    };
  }, [isCamOn]);

  // Manejo de la cámara (Activar / Desactivar)
  const toggleCamera = () => {
    if (isCamOn && cameraRef.current) {
      cameraRef.current.stop();
      setIsCamOn(false);
      setDetectedChoice(null);
    } else {
      setIsCamOn(true);
    }
  };

  // Iniciar la ronda de 20 segundos
  const startTurn = () => {
    setTimeLeft(20);
    setManualChoice(null);
    setDetectedChoice(null);
    setIsTurnActive(true);
  };

  // Cronómetro de 20 segundos
  useEffect(() => {
    let timer = null;
    if (isTurnActive && timeLeft > 0) {
      timer = setInterval(() => {
        setTimeLeft((prev) => prev - 1);
      }, 1000);
    } else if (timeLeft === 0 && isTurnActive) {
      setIsTurnActive(false);
      
      // Al agotar los 20 segundos, se envía la jugada manual o la detectada por la cámara
      const finalSelection = manualChoice || detectedChoice || 'NINGUNA (TIEMPO AGOTADO)';
      if (onPlaySelected) {
        onPlaySelected(finalSelection);
      }
    }

    return () => clearInterval(timer);
  }, [isTurnActive, timeLeft, manualChoice, detectedChoice, onPlaySelected]);

  // Selección manual de botón
  const handleManualSelect = (choice) => {
    setManualChoice(choice);
  };

  const currentSelection = manualChoice || detectedChoice;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '15px', padding: '20px', border: '2px solid #333', borderRadius: '12px', maxWidth: '400px', margin: '0 auto' }}>
      
      {/* 1. SECCIÓN DE CÁMARA */}
      <div style={{ textAlign: 'center' }}>
        <video
          ref={videoRef}
          autoPlay
          playsInline
          muted
          style={{ width: '280px', height: '210px', backgroundColor: '#111', borderRadius: '8px', transform: 'scaleX(-1)' }}
        />
        <div style={{ marginTop: '8px' }}>
          <button onClick={toggleCamera}>
            {isCamOn ? 'Desactivar Cámara' : 'Activar Cámara'}
          </button>
        </div>
      </div>

      {/* 2. RECONOCIMIENTO EN TIEMPO REAL */}
      {isCamOn && (
        <div style={{ fontSize: '16px', fontWeight: 'bold', color: '#007bff' }}>
          Detección por Cámara: {detectedChoice ? detectedChoice : 'Muestra tu mano a la cámara...'}
        </div>
      )}

      {/* 3. CRONÓMETRO DE 20 SEGUNDOS */}
      <div style={{ textAlign: 'center', width: '100%' }}>
        <h2 style={{ color: timeLeft <= 5 ? 'red' : '#333', margin: '5px 0' }}>
          Tiempo restante: {timeLeft}s
        </h2>
        
        <button onClick={startTurn} disabled={isTurnActive} style={{ padding: '8px 16px', margin: '8px 0', cursor: 'pointer' }}>
          {isTurnActive ? 'Turno en progreso...' : 'Iniciar Turno de Jugada (20s)'}
        </button>
      </div>

      {/* 4. OPCIONES MANUALE DE JUEGO */}
      <div style={{ textAlign: 'center', width: '100%' }}>
        <p style={{ margin: '5px 0', fontWeight: '600' }}>O elige manualmente:</p>
        <div style={{ display: 'flex', justifyContent: 'center', gap: '10px' }}>
          {['PIEDRA', 'PAPEL', 'TIJERA'].map((option) => (
            <button
              key={option}
              disabled={!isTurnActive}
              onClick={() => handleManualSelect(option)}
              style={{
                padding: '8px 12px',
                backgroundColor: manualChoice === option ? '#28a745' : '#e0e0e0',
                color: manualChoice === option ? '#fff' : '#000',
                border: 'none',
                borderRadius: '5px',
                cursor: isTurnActive ? 'pointer' : 'not-allowed'
              }}
            >
              {option}
            </button>
          ))}
        </div>
      </div>

      {/* 5. JUGADA SELECCIONADA */}
      <div style={{ marginTop: '10px', padding: '10px', backgroundColor: '#f8f9fa', width: '100%', textAlign: 'center', borderRadius: '6px' }}>
        <strong>Jugada Elegida: </strong> 
        <span style={{ color: '#28a745', fontWeight: 'bold' }}>
          {currentSelection ? currentSelection : 'Pendiente...'}
        </span>
      </div>

    </div>
  );
}